package auth

import (
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/LCBRST/webvpn/internal/netutil"
)

// Authenticate checks the session cookie and writes a challenge if it is
// missing or invalid. It returns the user on success.
//
// This runs in front of every proxied subdomain as well as the portal, not just
// the portal: an unauthenticated request to router-vpn.example.com must not
// reach the router.
func (h *Handlers) Authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(h.CookieName)
	if err != nil {
		h.challenge(w, r)
		return "", false
	}

	user, expiry, ok := h.Signer.Verify(c.Value)
	if !ok {
		h.clearSession(w)
		h.challenge(w, r)
		return "", false
	}

	// Sliding expiry: once a token is past the refresh point, hand back a fresh
	// one so that anyone still using the thing never gets logged out.
	if h.Signer.NeedsRefresh(expiry) {
		if token, err := h.Signer.Issue(user); err == nil {
			h.setSession(w, token)
		}
	}
	return user, true
}

// CurrentUser reports who is logged in, without writing anything to the
// response. Use Authenticate when you want the challenge behaviour.
func (h *Handlers) CurrentUser(r *http.Request) (string, bool) {
	c, err := r.Cookie(h.CookieName)
	if err != nil {
		return "", false
	}
	user, _, ok := h.Signer.Verify(c.Value)
	return user, ok
}

// challenge sends an unauthenticated caller to the login page.
func (h *Handlers) challenge(w http.ResponseWriter, r *http.Request) {
	// A fetch() or XHR must not be answered with a redirect. The browser would
	// follow it transparently and hand the login page's HTML to the caller as a
	// 200 response, which turns a simple "you are logged out" into a baffling
	// parse error somewhere deep in the frontend.
	if wantsJSON(r) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthenticated"}`)
		return
	}

	next := h.Scheme + "://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, h.portalURL("/login?next="+url.QueryEscape(next)), http.StatusFound)
}

// wantsJSON reports whether the caller is a script rather than a browser
// navigating to a page.
func wantsJSON(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		return true
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Mode"), "cors") {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// safeNext validates a post-login destination.
//
// Without this, /login?next=https://evil.example is a finished phishing page:
// the user sees our own login form, and the redirect that follows lands them
// somewhere else entirely.
func (h *Handlers) safeNext(raw string) string {
	if raw == "" {
		return h.portalURL("/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return h.portalURL("/")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return h.portalURL("/")
	}
	// Any subdomain of our own base domain is fine, which is what lets a login
	// that started at a service subdomain return there.
	if !netutil.UnderDomain(u.Host, h.CookieDomain) {
		return h.portalURL("/")
	}
	return u.String()
}

func (h *Handlers) portalURL(path string) string {
	return h.Scheme + "://" + h.PortalHost + path
}
