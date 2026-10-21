package netutil

import (
	"net/http/httptest"
	"testing"
)

func TestClientIPTrustsHeadersFromLoopback(t *testing.T) {
	r := httptest.NewRequest("POST", "http://vpn.example.com/login", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")

	if got := ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want the CF-Connecting-IP value", got)
	}
}

func TestClientIPIgnoresHeadersFromOtherPeers(t *testing.T) {
	// If the listener were ever exposed beyond loopback, a LAN peer could set
	// any bucket key it liked. The header must not be believed.
	r := httptest.NewRequest("POST", "http://vpn.example.com/login", nil)
	r.RemoteAddr = "192.168.1.20:54321"
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	r.Header.Set("X-Forwarded-For", "203.0.113.8")

	if got := ClientIP(r); got != "192.168.1.20" {
		t.Errorf("ClientIP = %q, want the socket peer", got)
	}
}

func TestClientIPFallsBackToRightmostXFF(t *testing.T) {
	r := httptest.NewRequest("POST", "http://vpn.example.com/login", nil)
	r.RemoteAddr = "[::1]:54321"
	r.Header.Set("X-Forwarded-For", "198.51.100.1, 203.0.113.9")

	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want the rightmost entry", got)
	}
}

func TestRateKeyGroupsIPv6ByPrefix(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		{"2001:db8:1:2::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:ffff::9", "2001:db8:1:2::/64"},
		{"not-an-address", "not-an-address"},
	}
	for _, tt := range tests {
		if got := RateKey(tt.in); got != tt.want {
			t.Errorf("RateKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
