package auth

import (
	"testing"
	"time"
)

func newTestLimiter(now *time.Time, p Policy) *Limiter {
	l := NewLimiter(p)
	l.now = func() time.Time { return *now }
	return l
}

func lockoutPolicy() Policy {
	return Policy{
		AttemptsPerMinute: 60, // large enough that the bucket never interferes
		MaxConcurrent:     1,
		MaxFailures:       3,
		FailureWindow:     15 * time.Minute,
		Lockout:           10 * time.Minute,
		MaxLockout:        time.Hour,
	}
}

func TestTokenBucketSpacesAttempts(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, Policy{AttemptsPerMinute: 5, MaxConcurrent: 1})

	for i := 0; i < 5; i++ {
		if v := l.Allow("ip"); !v.OK {
			t.Fatalf("attempt %d: refused with %v", i+1, v)
		}
	}

	v := l.Allow("ip")
	if v.OK {
		t.Fatal("sixth attempt was allowed, want the burst to be exhausted")
	}
	if v.Locked {
		t.Error("a bucket denial was reported as a lockout")
	}
	if v.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %v, want 12s", v.RetryAfter)
	}

	now = now.Add(12 * time.Second)
	if v := l.Allow("ip"); !v.OK {
		t.Fatalf("attempt after refill refused: %v", v)
	}
}

func TestFailuresLockTheClientOut(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}

	v := l.Allow("ip")
	if v.OK {
		t.Fatal("client was allowed immediately after reaching the failure threshold")
	}
	if !v.Locked {
		t.Error("refusal was not reported as a lockout")
	}
	if v.RetryAfter != 10*time.Minute {
		t.Errorf("RetryAfter = %v, want 10m", v.RetryAfter)
	}

	// Refreshing the page and trying again a little later must change nothing:
	// the lockout lives in the limiter, not in the page.
	now = now.Add(9 * time.Minute)
	if v := l.Allow("ip"); v.OK || !v.Locked {
		t.Fatalf("attempt during lockout: %v, want a lockout refusal", v)
	}

	now = now.Add(time.Minute + time.Second)
	if v := l.Allow("ip"); !v.OK {
		t.Fatalf("attempt after the lockout expired was refused: %v", v)
	}
}

func TestRepeatedLockoutsEscalate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	if v := l.Allow("ip"); v.RetryAfter != 10*time.Minute {
		t.Fatalf("first lockout = %v, want 10m", v.RetryAfter)
	}

	now = now.Add(10*time.Minute + time.Second)
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	if v := l.Allow("ip"); v.RetryAfter != 20*time.Minute {
		t.Fatalf("second lockout = %v, want 20m", v.RetryAfter)
	}

	now = now.Add(20*time.Minute + time.Second)
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	if v := l.Allow("ip"); v.RetryAfter != 40*time.Minute {
		t.Fatalf("third lockout = %v, want 40m", v.RetryAfter)
	}

	// The doubling is capped.
	now = now.Add(40*time.Minute + time.Second)
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	if v := l.Allow("ip"); v.RetryAfter != time.Hour {
		t.Fatalf("fourth lockout = %v, want the 1h cap", v.RetryAfter)
	}
}

func TestFailuresOutsideTheWindowDoNotCount(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	l.Fail("ip")
	l.Fail("ip")

	now = now.Add(15*time.Minute + time.Second)
	l.Fail("ip") // starts a fresh window

	if v := l.Allow("ip"); !v.OK {
		t.Fatalf("old failures still counted: %v", v)
	}
}

func TestSuccessDoesNotEraseFailures(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	// A client with some other valid account could log in after every wrong
	// guess to reset the counter if success cleared it. It must not.
	l.Fail("ip")
	l.Fail("ip")
	l.Succeed("ip")
	l.Fail("ip") // the third failure in the window still locks

	if v := l.Allow("ip"); v.OK || !v.Locked {
		t.Fatalf("success reset the failure count: %v", v)
	}
}

func TestSuccessRelaxesEscalation(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	now = now.Add(10*time.Minute + time.Second) // first lockout served
	l.Succeed("ip")                             // a real login in between
	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}

	// Without Succeed this would be the second offence, and 20m.
	if v := l.Allow("ip"); v.RetryAfter != 10*time.Minute {
		t.Fatalf("lockout after a success = %v, want the base 10m", v.RetryAfter)
	}
}

func TestFailDuringLockoutDoesNotEscalate(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}
	// A request admitted just before the lock landed reports its failure late.
	l.Fail("ip")

	if v := l.Allow("ip"); v.RetryAfter != 10*time.Minute {
		t.Fatalf("in-flight failure escalated the lockout: %v", v)
	}
}

func TestSweepKeepsLockedClients(t *testing.T) {
	now := time.Unix(1000, 0)
	p := lockoutPolicy()
	p.Lockout = 4 * time.Hour
	p.MaxLockout = 4 * time.Hour
	l := newTestLimiter(&now, p)

	for i := 0; i < 3; i++ {
		l.Fail("ip")
	}

	// Longer than the idle sweep threshold, shorter than the lockout. If the
	// sweeper dropped this entry a refresh would look like a fresh client.
	now = now.Add(35 * time.Minute)
	if v := l.Allow("ip"); v.OK || !v.Locked {
		t.Fatalf("locked client was swept away: %v", v)
	}
}

func TestLockoutsArePerKey(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newTestLimiter(&now, lockoutPolicy())

	for i := 0; i < 3; i++ {
		l.Fail("10.0.0.1")
	}
	if v := l.Allow("10.0.0.1"); v.OK {
		t.Fatal("locked key was allowed")
	}
	if v := l.Allow("10.0.0.2"); !v.OK {
		t.Fatalf("unrelated key was refused: %v", v)
	}
}

func TestVerdictMessageAndRetrySeconds(t *testing.T) {
	locked := Verdict{RetryAfter: 90 * time.Second, Locked: true}
	if got := locked.RetrySeconds(); got != 90 {
		t.Errorf("RetrySeconds = %d, want 90", got)
	}
	if msg := locked.Message(); msg == "" {
		t.Error("locked verdict has no message")
	}

	throttled := Verdict{RetryAfter: 500 * time.Millisecond}
	if got := throttled.RetrySeconds(); got != 1 {
		t.Errorf("RetrySeconds = %d, want 1 (rounded up)", got)
	}
	if msg := throttled.Message(); msg == "" {
		t.Error("throttled verdict has no message")
	}
}
