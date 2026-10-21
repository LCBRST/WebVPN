// Package auth handles login: verifying passwords and issuing the session
// token that every proxied subdomain shares.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// claims is the payload carried inside a session token. Field names are short
// because the whole thing travels in a cookie on every request.
type claims struct {
	User  string `json:"u"`
	Exp   int64  `json:"e"`  // UTC unix seconds
	Epoch int    `json:"ep"` // matches Session.Epoch in the config
	Nonce string `json:"n"`  // makes every issued token distinct
}

// Signer issues and verifies stateless session tokens.
//
// Stateless rather than a server-side session table, because surviving a
// restart matters more here than being able to revoke one specific session. The
// revoke-everything case is covered by bumping Session.Epoch and restarting,
// which is the one that actually comes up.
type Signer struct {
	key          []byte
	ttl          time.Duration
	refreshAfter time.Duration
	epoch        int

	// now is a seam for tests. Session expiry is wall-clock dependent and a
	// machine with a drifting clock is a real failure mode on home networks,
	// so being able to move it is worth the one field.
	now func() time.Time
}

func NewSigner(key []byte, ttl, refreshAfter time.Duration, epoch int) *Signer {
	return &Signer{
		key:          key,
		ttl:          ttl,
		refreshAfter: refreshAfter,
		epoch:        epoch,
		now:          time.Now,
	}
}

// TTL is how long a freshly issued token lives.
func (s *Signer) TTL() time.Duration { return s.ttl }

// Issue returns a signed token for user.
func (s *Signer) Issue(user string) (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	payload, err := json.Marshal(claims{
		User:  user,
		Exp:   s.now().UTC().Add(s.ttl).Unix(),
		Epoch: s.epoch,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}

	body := base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(s.mac(body)), nil
}

// Verify returns the user the token belongs to and when it expires. A token is
// rejected if the signature does not match, if it was issued under a different
// epoch, or if it has expired.
func (s *Signer) Verify(token string) (string, time.Time, bool) {
	body, sig, ok := strings.Cut(token, ".")
	if !ok || body == "" || sig == "" {
		return "", time.Time{}, false
	}

	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return "", time.Time{}, false
	}
	// hmac.Equal is constant time, so the signature cannot be recovered byte
	// by byte from response timings.
	if !hmac.Equal(got, s.mac(body)) {
		return "", time.Time{}, false
	}

	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return "", time.Time{}, false
	}
	var c claims
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", time.Time{}, false
	}

	expiry := time.Unix(c.Exp, 0).UTC()
	if c.Epoch != s.epoch || !s.now().UTC().Before(expiry) {
		return "", time.Time{}, false
	}
	return c.User, expiry, true
}

// NeedsRefresh reports whether a token is far enough through its life that it
// should be reissued on this request, which is what turns a fixed deadline into
// a sliding one for anyone who keeps using the thing.
func (s *Signer) NeedsRefresh(expiry time.Time) bool {
	return s.now().UTC().Add(s.refreshAfter).After(expiry)
}

func (s *Signer) mac(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
