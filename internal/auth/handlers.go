package auth

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/LCBRST/webvpn/internal/netutil"
	"github.com/LCBRST/webvpn/internal/portal"
)

// Users verifies a password.
//
// An interface rather than the concrete store, because the accounts it is
// checking against are editable while the process runs: the implementation
// reads the current state on each call, so adding a user or changing a password
// takes effect on the next login rather than the next restart.
//
// The implementation is expected to spend the same time on an unknown name as
// on a known one. See the note in internal/store.
type Users interface {
	Authenticate(name, password string) bool
}

// Handlers serves the login page, the login POST, and logout.
type Handlers struct {
	Users   Users
	Signer  *Signer
	Views   *portal.Views
	Limiter *Limiter
	Logger  *slog.Logger

	CookieName string
	// CookieDomain is the base domain, not the portal host. It has to be the
	// parent so that the session cookie is sent to every service subdomain too.
	CookieDomain string
	CookieSecure bool

	// Scheme and PortalHost are used to build absolute URLs back to the portal.
	Scheme     string
	PortalHost string
}

// ServeLoginPage handles GET /login.
func (h *Handlers) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.CurrentUser(r); ok {
		http.Redirect(w, r, h.portalURL("/"), http.StatusFound)
		return
	}
	h.Views.RenderLogin(w, http.StatusOK, portal.LoginData{
		Next: h.safeNext(r.URL.Query().Get("next")),
	})
}

// ServeLogin handles POST /login.
func (h *Handlers) ServeLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Validate before using it anywhere, including in the form on an error
	// re-render.
	next := h.safeNext(r.PostFormValue("next"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	ip := netutil.ClientIP(r)
	// RateKey, not ip: IPv6 /64 regrouping happens here, and the same value has
	// to be used for Allow, Fail and Succeed or the failure count lands on a
	// different entry than the one that admitted the attempt.
	key := netutil.RateKey(ip)

	if v := h.Limiter.Allow(key); !v.OK {
		if s := v.RetrySeconds(); s > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s))
		}
		h.Logger.Warn("login throttled", "ip", ip, "locked", v.Locked, "retry_after", v.RetryAfter.String())
		h.Views.RenderLogin(w, http.StatusTooManyRequests, portal.LoginData{
			Error: v.Message(),
			Next:  next,
		})
		return
	}

	// Serialise the bcrypt comparison. Everything before this point is cheap,
	// and this is the step that costs a quarter second of CPU.
	release, ok := h.Limiter.Acquire(r.Context())
	if !ok {
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return
	}
	matched := h.Users.Authenticate(username, password)
	release()

	if !matched {
		// The attempted name is logged; the password never is.
		h.Limiter.Fail(key)
		h.Logger.Warn("login failed", "ip", ip, "user", username)
		h.Views.RenderLogin(w, http.StatusUnauthorized, portal.LoginData{
			Error: "用户名或密码不正确。",
			Next:  next,
		})
		return
	}
	h.Limiter.Succeed(key)

	token, err := h.Signer.Issue(username)
	if err != nil {
		h.Logger.Error("issue session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.setSession(w, token)
	h.Logger.Info("login ok", "ip", ip, "user", username)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// ServeLogout handles GET /logout.
func (h *Handlers) ServeLogout(w http.ResponseWriter, r *http.Request) {
	if user, ok := h.CurrentUser(r); ok {
		h.Logger.Info("logout", "ip", netutil.ClientIP(r), "user", user)
	}
	h.clearSession(w)
	http.Redirect(w, r, h.portalURL("/login"), http.StatusSeeOther)
}

func (h *Handlers) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:  h.CookieName,
		Value: token,
		Path:  "/",
		// Naming the parent domain is what makes one login cover every service
		// subdomain. A cookie set from vpn.example.com is allowed to name
		// example.com; it could not name anything outside it.
		Domain:   h.CookieDomain,
		HttpOnly: true,
		Secure:   h.CookieSecure,
		// Service subdomains count as same-site with the portal, so Lax is
		// sufficient and avoids the None-with-Secure pairing.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.Signer.TTL().Seconds()),
	})
}

func (h *Handlers) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     h.CookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.CookieDomain,
		HttpOnly: true,
		Secure:   h.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
