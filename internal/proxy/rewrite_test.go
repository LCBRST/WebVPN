package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"testing"

	"github.com/LCBRST/webvpn/internal/config"
)

// applyRewrite drives the Rewrite hook with a request shaped like one
// cloudflared would hand us, and returns what the backend would receive.
func applyRewrite(t *testing.T, svc *config.Service, target string, headers map[string]string) *http.Request {
	t.Helper()

	in := httptest.NewRequest(http.MethodGet, "https://app-vpn.example.com/socket.io/", nil)
	in.Host = "app-vpn.example.com"
	for k, v := range headers {
		in.Header.Set(k, v)
	}

	out := in.Clone(in.Context())
	newRewrite(svc, mustURL(t, target), "webvpn_session")(&httputil.ProxyRequest{In: in, Out: out})
	return out
}

func TestRewriteOrigin(t *testing.T) {
	const target = "http://192.168.1.10:8090"

	tests := []struct {
		name   string
		svc    *config.Service
		origin string
		want   string // "" means the header must come out absent
	}{
		{
			name:   "off by default, the browser origin passes through",
			svc:    &config.Service{},
			origin: "https://app-vpn.example.com",
			want:   "https://app-vpn.example.com",
		},
		{
			name:   "on, replaced with the origin the backend computes for itself",
			svc:    &config.Service{RewriteOrigin: true},
			origin: "https://app-vpn.example.com",
			want:   "http://192.168.1.10:8090",
		},
		{
			name:   "on, preserve_host changes the host the backend sees",
			svc:    &config.Service{RewriteOrigin: true, PreserveHost: true},
			origin: "https://app-vpn.example.com",
			want:   "http://app-vpn.example.com",
		},
		{
			name:   "on, but no Origin was sent, so none is invented",
			svc:    &config.Service{RewriteOrigin: true},
			origin: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.origin != "" {
				headers["Origin"] = tt.origin
			}

			if got := applyRewrite(t, tt.svc, target, headers).Header.Get("Origin"); got != tt.want {
				t.Errorf("Origin = %q, want %q", got, tt.want)
			}
		})
	}
}

// The point of the option is a scheme the browser cannot produce: the tunnel
// dials the origin over plain http, so the backend never sees the https the
// browser used. That has to survive a target that is not on port 80.
func TestRewriteOriginKeepsTargetScheme(t *testing.T) {
	svc := &config.Service{RewriteOrigin: true}

	out := applyRewrite(t, svc, "https://192.168.1.10:8443", map[string]string{
		"Origin": "https://app-vpn.example.com",
	})

	if got, want := out.Header.Get("Origin"), "https://192.168.1.10:8443"; got != want {
		t.Errorf("Origin = %q, want %q", got, want)
	}
}
