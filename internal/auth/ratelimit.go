package auth

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// Policy configures a Limiter.
type Policy struct {
	// AttemptsPerMinute is the sustained guess rate per client, and also the
	// size of the initial burst.
	AttemptsPerMinute int
	// MaxConcurrent caps password comparisons running at once. bcrypt at cost
	// 12 is roughly 250ms of solid CPU; without this, a handful of concurrent
	// POSTs is enough to saturate the machine.
	MaxConcurrent int
	// MaxFailures wrong passwords within FailureWindow lock the client out.
	MaxFailures   int
	FailureWindow time.Duration
	// Lockout is the first cooling-off period. Every further lockout without a
	// successful login in between doubles it, up to MaxLockout.
	Lockout    time.Duration
	MaxLockout time.Duration
}

// Verdict is the answer to an attempt: allowed, or refused with a reason.
type Verdict struct {
	OK bool
	// RetryAfter is how long until the client may try again. Zero when OK.
	RetryAfter time.Duration
	// Locked distinguishes "too many wrong passwords" from "trying too fast".
	// The latter clears by itself as tokens refill; the former does not, which
	// is exactly what makes a page refresh useless to an attacker.
	Locked bool
}

// RetrySeconds is RetryAfter rounded up to whole seconds, for a Retry-After
// header.
func (v Verdict) RetrySeconds() int {
	if v.RetryAfter <= 0 {
		return 0
	}
	return int(math.Ceil(v.RetryAfter.Seconds()))
}

// Message is a user-facing explanation of the refusal.
func (v Verdict) Message() string {
	if v.Locked {
		if v.RetryAfter < time.Minute {
			return "密码错误次数过多，请稍后再试。"
		}
		return fmt.Sprintf("密码错误次数过多，已暂时锁定，请 %d 分钟后再试。",
			int(math.Ceil(v.RetryAfter.Minutes())))
	}
	return "尝试过于频繁，请稍后再试。"
}

// Limiter throttles the login endpoint.
//
// Three mechanisms, and the order they are applied in matters:
//
//   - A failure lockout per client address. Once MaxFailures wrong passwords
//     land inside FailureWindow, every attempt is refused for Lockout, and the
//     period doubles on each repeat offence. This state lives in the server, so
//     refreshing the page and submitting again does not clear it.
//   - A token bucket per client address spaces out repeated guesses below that
//     threshold, so an attacker cannot burn through the failure budget faster
//     than AttemptsPerMinute.
//   - A semaphore allows only one password comparison at a time. Since the
//     bucket key is already degraded to a single global key behind the tunnel
//     when CF-Connecting-IP is absent, this is the limit that does the real
//     work in that case.
//
// User-name enumeration is handled separately, in UserStore.
type Limiter struct {
	mu      sync.Mutex
	clients map[string]*client

	rate  float64 // tokens added per second
	burst float64 // bucket capacity

	maxFailures   int
	failureWindow time.Duration
	lockout       time.Duration
	maxLockout    time.Duration

	lastSweep time.Time

	sem chan struct{}
	now func() time.Time
}

type client struct {
	tokens float64
	last   time.Time

	failures     int
	failureStart time.Time
	lockedUntil  time.Time
	// lockouts counts the lockout periods served since the last successful
	// login, and drives the exponential backoff.
	lockouts int

	seen time.Time
}

// NewLimiter builds a limiter from p. Missing policy values are filled with
// the same defaults the config file uses, so a zero Policy is still safe.
func NewLimiter(p Policy) *Limiter {
	if p.AttemptsPerMinute < 1 {
		p.AttemptsPerMinute = 5
	}
	if p.MaxConcurrent < 1 {
		p.MaxConcurrent = 1
	}
	if p.MaxFailures < 1 {
		p.MaxFailures = 5
	}
	if p.FailureWindow <= 0 {
		p.FailureWindow = 15 * time.Minute
	}
	if p.Lockout <= 0 {
		p.Lockout = 15 * time.Minute
	}
	if p.MaxLockout < p.Lockout {
		p.MaxLockout = p.Lockout
	}
	return &Limiter{
		clients:       make(map[string]*client),
		rate:          float64(p.AttemptsPerMinute) / 60,
		burst:         float64(p.AttemptsPerMinute),
		maxFailures:   p.MaxFailures,
		failureWindow: p.FailureWindow,
		lockout:       p.Lockout,
		maxLockout:    p.MaxLockout,
		sem:           make(chan struct{}, p.MaxConcurrent),
		now:           time.Now,
	}
}

// Allow reports whether this attempt may proceed, consuming a token when it
// does. A refusal says how long to wait and whether it is a lockout.
func (l *Limiter) Allow(key string) Verdict {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.sweepLocked(now)

	c := l.clientLocked(key, now)

	// The lockout is checked first and does not touch the bucket: otherwise a
	// long lockout would refill the bucket anyway, and the point is to grant
	// nothing at all while it lasts.
	if now.Before(c.lockedUntil) {
		return Verdict{RetryAfter: c.lockedUntil.Sub(now), Locked: true}
	}

	c.tokens = min(l.burst, c.tokens+now.Sub(c.last).Seconds()*l.rate)
	c.last = now

	if c.tokens < 1 {
		wait := time.Duration((1 - c.tokens) / l.rate * float64(time.Second))
		return Verdict{RetryAfter: wait}
	}
	c.tokens--
	return Verdict{OK: true}
}

// Fail records a wrong password for key, locking it out when the failure
// threshold is reached.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	c := l.clientLocked(key, now)

	// Requests that were already in flight when the lockout was set must not
	// extend or escalate it.
	if now.Before(c.lockedUntil) {
		return
	}

	// Fixed window: failures older than FailureWindow no longer count. The
	// window is reset rather than slid, which is enough here and keeps the
	// state to two fields.
	if c.failureStart.IsZero() || now.Sub(c.failureStart) > l.failureWindow {
		c.failureStart = now
		c.failures = 0
	}
	c.failures++
	if c.failures < l.maxFailures {
		return
	}

	c.lockedUntil = now.Add(l.lockoutFor(c.lockouts))
	c.lockouts++
	// Reset the streak, so serving the lockout buys the attacker no head start
	// on the next threshold.
	c.failures = 0
	c.failureStart = time.Time{}
}

// Succeed records a correct password.
//
// It deliberately does not clear the failure count or an active lockout. The
// address is shared: a client holding any valid account could otherwise make
// MaxFailures-1 wrong guesses, log in as itself, and repeat forever. Leaving
// the count alone means the failure window still has to expire. Only the
// escalation counter is relaxed, so a person who simply typo'd does not carry
// a doubled lockout around forever.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	c := l.clientLocked(key, now)
	if now.Before(c.lockedUntil) {
		return
	}
	c.lockouts = 0
}

func (l *Limiter) clientLocked(key string, now time.Time) *client {
	c, ok := l.clients[key]
	if !ok {
		c = &client{tokens: l.burst, last: now}
		l.clients[key] = c
	}
	c.seen = now
	return c
}

// lockoutFor is the n-th cooling-off period: Lockout doubled n times, capped.
func (l *Limiter) lockoutFor(strikes int) time.Duration {
	d := l.lockout
	for i := 0; i < strikes; i++ {
		if d >= l.maxLockout/2 {
			return l.maxLockout
		}
		d *= 2
	}
	return min(d, l.maxLockout)
}

// sweepLocked drops clients that have been idle long enough to be
// indistinguishable from a fresh one. An entry that is still locked out is
// kept even if it has been quiet, or the lockout could be swept away early.
func (l *Limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < 10*time.Minute {
		return
	}
	l.lastSweep = now
	for key, c := range l.clients {
		if now.Sub(c.seen) > 30*time.Minute && !now.Before(c.lockedUntil) {
			delete(l.clients, key)
		}
	}
}

// Acquire blocks until a password comparison slot is free. The returned
// function must be called to release it. It reports false if ctx ends first.
func (l *Limiter) Acquire(ctx context.Context) (func(), bool) {
	select {
	case l.sem <- struct{}{}:
		return func() { <-l.sem }, true
	case <-ctx.Done():
		return nil, false
	}
}
