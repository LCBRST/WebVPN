package proxy

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/LCBRST/webvpn/internal/config"
	"github.com/LCBRST/webvpn/internal/netutil"
)

// newRewrite builds the httputil.ReverseProxy Rewrite hook.
//
// Rewrite has a behaviour that is easy to miss: when it is set, net/http
// deletes the inbound Forwarded, X-Forwarded-For, X-Forwarded-Host and
// X-Forwarded-Proto headers from the outbound request before calling us, so a
// client cannot smuggle its own values through to the backend. The consequence
// is that everything a backend needs has to be written from scratch here, out
// of pr.In, which cloudflared filled in on a connection the client cannot
// reach.
func newRewrite(svc *config.Service, target *url.URL, sessionCookieName string) func(*httputil.ProxyRequest) {
	return func(pr *httputil.ProxyRequest) {
		// SetURL copies scheme, host and path onto the outbound request and
		// clears Out.Host, so the request goes out with the target's host in
		// the Host header. That is what most appliances want, since they
		// validate Host against their own address.
		pr.SetURL(target)

		if svc.PreserveHost {
			// Must come after SetURL, which would otherwise overwrite it.
			pr.Out.Host = pr.In.Host
		}

		if svc.RewriteOrigin {
			// Order matters here too: the value written below is built from
			// the Host set just above.
			rewriteOrigin(pr)
		}

		if svc.ShouldStripSessionCookie() && sessionCookieName != "" {
			// Our session token is for this proxy and nothing else. Leaving it
			// in means every appliance we proxy gets a credential it could
			// replay elsewhere, and their firmware is not something we audit.
			stripCookieByName(pr.Out.Header, sessionCookieName)
		}

		scheme := netutil.ClientScheme(pr.In)
		clientIP := netutil.ClientIP(pr.In)

		pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
		pr.Out.Header.Set("X-Forwarded-Proto", scheme)
		pr.Out.Header.Set("X-Real-IP", clientIP)
		if scheme == "https" {
			pr.Out.Header.Set("X-Forwarded-Port", "443")
		} else {
			pr.Out.Header.Set("X-Forwarded-Port", "80")
		}

		// Some appliances build their own IP allowlist out of X-Forwarded-For
		// and will reject a public address with a 403. That is the only reason
		// this is switchable.
		if svc.ShouldSendXFF() {
			pr.Out.Header.Set("X-Forwarded-For", clientIP)
		}
	}
}

// rewriteOrigin replaces the browser's Origin header with the one the backend
// computes for itself, which is what an application that validates Origin
// against its own host ends up comparing against.
//
// The scheme is the whole reason this is needed: the tunnel speaks plain HTTP
// to the origin, so a backend deriving its origin from the request connection
// sees "http://<its host>" while the browser, correctly, sent
// "https://<public host>". Servers that check this reject the request, and
// typically only on the WebSocket handshake — which is why the symptom is a
// page that loads fine and a backend that never connects.
//
// The value is derived from the URL we are dialing and the Host we are sending
// rather than from a fixed string, so it stays correct under preserve_host,
// which changes the Host the backend sees.
//
// Opt-in, because it also tells the backend that a request which is not
// same-origin is same-origin, which is exactly the signal a CSRF check relies
// on. Only worth it for a service that is otherwise unusable.
func rewriteOrigin(pr *httputil.ProxyRequest) {
	// An absent Origin means same-origin as far as the browser is concerned,
	// and these servers read it the same way. Inventing one would turn a
	// request the backend accepts into one it rejects.
	if pr.Out.Header.Get("Origin") == "" || pr.Out.URL == nil {
		return
	}
	// SetURL leaves Out.Host empty rather than filling it in, so an empty value
	// means the transport will use the target's host. Reading Out.Host alone
	// would produce a bare "http://".
	host := pr.Out.Host
	if host == "" {
		host = pr.Out.URL.Host
	}
	pr.Out.Header.Set("Origin", pr.Out.URL.Scheme+"://"+host)
}

// stripCookieByName removes one cookie from a Cookie header, leaving the rest
// alone. Deleting the whole header would be simpler and wrong: the backend's
// own session cookie travels in the same header and losing it would break the
// login we are trying to proxy.
//
// Request Cookie headers are a semicolon-separated list of name=value pairs
// with no attributes, and a cookie value may not contain a semicolon, so
// splitting on ";" is safe here. (Splitting Set-Cookie on "," never is; see
// modify.go for why.)
func stripCookieByName(h http.Header, name string) {
	lines := h.Values("Cookie")
	if len(lines) == 0 {
		return
	}
	h.Del("Cookie")

	for _, line := range lines {
		kept := make([]string, 0, 8)
		for _, pair := range strings.Split(line, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			if k, _, ok := strings.Cut(pair, "="); ok && strings.TrimSpace(k) == name {
				continue
			}
			kept = append(kept, pair)
		}
		if len(kept) > 0 {
			h.Add("Cookie", strings.Join(kept, "; "))
		}
	}
}
