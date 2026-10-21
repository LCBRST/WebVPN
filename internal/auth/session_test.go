package auth

import (
	"strings"
	"testing"
	"time"
)

func testSigner(epoch int) *Signer {
	return NewSigner([]byte("a-test-signing-key-of-sufficient-length"), time.Hour, 30*time.Minute, epoch)
}

func TestIssueAndVerifyRoundTrip(t *testing.T) {
	s := testSigner(1)

	token, err := s.Issue("admin")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	user, expiry, ok := s.Verify(token)
	if !ok {
		t.Fatal("a freshly issued token did not verify")
	}
	if user != "admin" {
		t.Errorf("user = %q, want admin", user)
	}
	if !expiry.After(time.Now()) {
		t.Errorf("expiry %v is not in the future", expiry)
	}
}

func TestIssueProducesDistinctTokens(t *testing.T) {
	// Two logins in the same second must not produce the same token, which is
	// what the nonce is for.
	s := testSigner(1)

	first, _ := s.Issue("admin")
	second, _ := s.Issue("admin")
	if first == second {
		t.Error("tokens issued for the same user should differ")
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	s := testSigner(1)
	token, _ := s.Issue("admin")

	body, sig, _ := strings.Cut(token, ".")
	// Swap the payload for a different user, keeping the original signature.
	forged, err := s.Issue("root")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	forgedBody, _, _ := strings.Cut(forged, ".")
	tampered := forgedBody + "." + sig

	if _, _, ok := s.Verify(tampered); ok {
		t.Fatalf("tampered token verified: %q/%q", body, forgedBody)
	}
}

func TestVerifyRejectsWrongEpoch(t *testing.T) {
	// Bumping the epoch is the "log everybody out" switch, so a token from the
	// old epoch has to stop working.
	token, err := testSigner(1).Issue("admin")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, _, ok := testSigner(2).Verify(token); ok {
		t.Error("a token from a previous epoch verified")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s := NewSigner([]byte("a-test-signing-key-of-sufficient-length"), time.Minute, 30*time.Second, 1)
	token, err := s.Issue("admin")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Move the clock past the expiry rather than sleeping for a minute.
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }

	if _, _, ok := s.Verify(token); ok {
		t.Error("an expired token verified")
	}
}

func TestNeedsRefresh(t *testing.T) {
	s := NewSigner([]byte("a-test-signing-key-of-sufficient-length"), time.Hour, 30*time.Minute, 1)

	if s.NeedsRefresh(time.Now().Add(time.Hour)) {
		t.Error("a fresh token should not need refreshing")
	}
	if !s.NeedsRefresh(time.Now().Add(10 * time.Minute)) {
		t.Error("a token past the refresh point should need refreshing")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	s := testSigner(1)

	for _, token := range []string{"", ".", "nodot", "a.b", "!!.!!", "a."} {
		if _, _, ok := s.Verify(token); ok {
			t.Errorf("Verify(%q) accepted a malformed token", token)
		}
	}
}
