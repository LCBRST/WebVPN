package proxy

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/LCBRST/webvpn/internal/config"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestToPublicRef(t *testing.T) {
	target := mustURL(t, "http://192.168.1.1")
	svc := &config.Service{ExtraLocationHosts: []string{"router.lan"}}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"backend address becomes relative", "http://192.168.1.1/login", "/login"},
		{"path with port in target", "http://192.168.1.1:80/x", "/x"},
		{"query and fragment survive", "http://192.168.1.1/a?b=1#c", "/a?b=1#c"},
		{"empty path becomes root", "http://192.168.1.1", "/"},
		{"extra host counts as the backend", "http://router.lan/admin", "/admin"},
		{"already relative is left alone", "/already/relative", "/already/relative"},
		{"third party is left alone", "https://example.org/x", "https://example.org/x"},
		{"unparsable is left alone", "http://[::1", "http://[::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toPublicRef(tt.in, svc, target); got != tt.want {
				t.Errorf("toPublicRef(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsBackendHostMatchesTargetWithPort(t *testing.T) {
	target := mustURL(t, "http://192.168.1.1:8080")
	svc := &config.Service{}

	if !isBackendHost("192.168.1.1:8080", svc, target) {
		t.Error("expected the target's host:port to match")
	}
	// The hostname alone is what target.Hostname() returns, and appliances
	// redirect to it without the port as often as with it.
	if !isBackendHost("192.168.1.1", svc, target) {
		t.Error("expected the bare target host to match")
	}
	if isBackendHost("192.168.1.2", svc, target) {
		t.Error("a different address must not match")
	}
}

func TestRewriteSetCookieDropsDomain(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "sid=abc123; Domain=192.168.1.1; Path=/; HttpOnly")

	rewriteSetCookie(h)

	got := h.Get("Set-Cookie")
	if got == "" {
		t.Fatal("cookie was dropped entirely")
	}
	if strings.Contains(got, "Domain") {
		t.Errorf("Domain should have been dropped, got %q", got)
	}
	if !strings.Contains(got, "sid=abc123") || !strings.Contains(got, "HttpOnly") {
		t.Errorf("cookie was mangled, got %q", got)
	}
}

func TestRewriteSetCookieHandlesExpiresComma(t *testing.T) {
	// The comma inside Expires is what breaks any implementation that splits
	// the Set-Cookie header on ",". Two cookies, one with a date, is the case
	// that catches it.
	h := http.Header{}
	h.Add("Set-Cookie", "a=1; Expires=Wed, 21 Oct 2015 07:28:00 GMT; Domain=192.168.1.1")
	h.Add("Set-Cookie", "b=2; Domain=192.168.1.1")

	rewriteSetCookie(h)

	got := h.Values("Set-Cookie")
	if len(got) != 2 {
		t.Fatalf("expected 2 cookies, got %d: %q", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if strings.Contains(joined, "Domain") {
		t.Errorf("Domain survived: %q", joined)
	}
	if !strings.Contains(joined, "a=1") || !strings.Contains(joined, "b=2") {
		t.Errorf("a cookie was lost: %q", joined)
	}
	if !strings.Contains(joined, "Expires=Wed, 21 Oct 2015") {
		t.Errorf("Expires was mangled: %q", joined)
	}
}

func TestRewriteSetCookieRepairsPrefixContracts(t *testing.T) {
	// A browser silently discards these when the prefix rules are broken, so
	// the attributes are forced rather than passed through.
	h := http.Header{}
	h.Add("Set-Cookie", "__Host-sid=abc; Domain=192.168.1.1; Path=/admin")
	h.Add("Set-Cookie", "__Secure-tok=abc; Domain=192.168.1.1")

	rewriteSetCookie(h)

	joined := strings.Join(h.Values("Set-Cookie"), " | ")
	if !strings.Contains(joined, "Secure") {
		t.Errorf("Secure was not forced onto a prefixed cookie: %q", joined)
	}
	if !strings.Contains(joined, "__Host-sid=abc; Path=/") {
		t.Errorf("__Host- cookie should have Path forced to /, got %q", joined)
	}
}

func TestRewriteSetCookieKeepsUnparsableHeader(t *testing.T) {
	h := http.Header{}
	h.Add("Set-Cookie", "this is not a cookie")

	rewriteSetCookie(h)

	if got := h.Get("Set-Cookie"); got != "this is not a cookie" {
		t.Errorf("unparsable header should pass through untouched, got %q", got)
	}
}

func TestStripCookieByNameKeepsTheRest(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "webvpn_session=ours; device_session=theirs")

	stripCookieByName(h, "webvpn_session")

	got := h.Get("Cookie")
	if strings.Contains(got, "webvpn_session") {
		t.Errorf("session cookie was forwarded, got %q", got)
	}
	// The appliance's own cookie must survive, or its login breaks.
	if !strings.Contains(got, "device_session=theirs") {
		t.Errorf("the backend's cookie was lost, got %q", got)
	}
}

func TestStripCookieByNameRemovesHeaderWhenNothingLeft(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "webvpn_session=ours")

	stripCookieByName(h, "webvpn_session")

	if got := h.Get("Cookie"); got != "" {
		t.Errorf("expected no Cookie header, got %q", got)
	}
}

func TestFixCORSReplacesBackendOrigin(t *testing.T) {
	target := mustURL(t, "http://192.168.1.1")
	res := &http.Response{Header: http.Header{}}
	res.Header.Set("Access-Control-Allow-Origin", "http://192.168.1.1")
	res.Header.Set("Access-Control-Allow-Credentials", "true")
	res.Request = &http.Request{Header: http.Header{}}
	res.Request.Header.Set("Origin", "https://app-vpn.example.com")

	fixCORS(res, target)

	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://app-vpn.example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the request origin", got)
	}
	if got := res.Header.Get("Access-Control-Allow-Origin"); got == "*" {
		t.Error("must not use a wildcard alongside credentials")
	}
}
