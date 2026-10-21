// Package app wires the config, store, auth and proxy packages into one handler
// and routes requests by Host.
package app

import (
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/LCBRST/webvpn/internal/auth"
	"github.com/LCBRST/webvpn/internal/config"
	"github.com/LCBRST/webvpn/internal/netutil"
	"github.com/LCBRST/webvpn/internal/portal"
	"github.com/LCBRST/webvpn/internal/proxy"
	"github.com/LCBRST/webvpn/internal/store"
)

// limiterPolicy bridges the config file's login section and the auth package,
// which deliberately does not import config. Both listeners build their own
// Limiter from the same values but keep separate state.
func limiterPolicy(cfg *config.Config) auth.Policy {
	return auth.Policy{
		AttemptsPerMinute: cfg.Login.AttemptsPerMinute,
		MaxConcurrent:     cfg.Login.MaxConcurrent,
		MaxFailures:       cfg.Login.MaxFailures,
		FailureWindow:     cfg.Login.FailureWindow.Std(),
		Lockout:           cfg.Login.Lockout.Std(),
		MaxLockout:        cfg.Login.MaxLockout.Std(),
	}
}

type App struct {
	cfg   *config.Config
	auth  *auth.Handlers
	views *portal.Views
	store *store.Store

	// lan is the admin page's own session machinery, separate from the portal's.
	// See admin.go for why it is not the same Handlers.
	lan *lanAuth

	// services is replaced whole when the service list changes, so a request
	// reads one consistent map without taking a lock, and a rebuild in progress
	// is never observable half-applied.
	services atomic.Pointer[map[string]serviceEntry]

	portalMux *http.ServeMux
	// lanMux is what the LAN listener serves. Separate from portalMux because
	// the two share no routes at all: the portal has sessions and proxying, the
	// LAN side has neither.
	lanMux *http.ServeMux
	logger *slog.Logger
}

// serviceEntry pairs a proxy handler with the service it was built from.
//
// The service is needed because authorization is decided per service subdomain,
// while the map is keyed by the full public host name.
type serviceEntry struct {
	svc     *config.Service
	handler *proxy.Handler
}

func New(cfg *config.Config, logger *slog.Logger) (*App, error) {
	views, err := portal.NewViews()
	if err != nil {
		return nil, err
	}

	// Before anything else that needs the service list: the store is what
	// decides whether the config file's users and services are the truth or
	// only the seed they were seeded from.
	st, err := store.New(cfg, logger)
	if err != nil {
		return nil, err
	}

	a := &App{cfg: cfg, views: views, store: st, logger: logger}

	a.auth = &auth.Handlers{
		// The store satisfies auth.Users, so a password change lands on the next
		// login rather than the next restart.
		Users:   st,
		Signer:  auth.NewSigner(cfg.SessionKey(), cfg.Session.TTL.Std(), cfg.Session.RefreshAfter.Std(), cfg.Session.Epoch),
		Views:   views,
		Limiter: auth.NewLimiter(limiterPolicy(cfg)),
		Logger:  logger,

		CookieName: cfg.Session.CookieName,
		// The base domain, so the cookie covers every service subdomain.
		CookieDomain: cfg.Server.BaseDomain,
		CookieSecure: cfg.Server.PublicScheme == "https",

		Scheme:     cfg.Server.PublicScheme,
		PortalHost: cfg.Server.PortalHost,
	}
	a.lan = newLANAuth(cfg)

	// Every listener is served from the same map, so this runs once at startup
	// and again after every edit that changes the service list.
	a.rebuild()

	// The portal goes through a ServeMux; proxied traffic deliberately does
	// not. ServeMux redirects //, cleans up .., and decodes %2F before the
	// handler ever sees the request, and appliances that sign or route on the
	// exact path they were asked for break when that happens.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", a.auth.ServeLoginPage)
	mux.HandleFunc("POST /login", a.auth.ServeLogin)
	mux.HandleFunc("GET /logout", a.auth.ServeLogout)
	mux.HandleFunc("GET /", a.handlePortal)
	a.portalMux = mux

	// The LAN listener. Only these paths exist; everything else falls through
	// to the mux's own 404, which is the same discipline as before -- there is
	// nothing here for a scanner to find by walking.
	//
	// {$} rather than / so that the index is the root and only the root. A
	// pattern of "/" would be a subtree and would answer every unknown path
	// with the index.
	lan := http.NewServeMux()
	lan.HandleFunc("GET /{$}", a.handleLANIndex)
	lan.HandleFunc("GET /admin/login", a.handleAdminLoginPage)
	lan.HandleFunc("POST /admin/login", a.handleAdminLogin)
	lan.HandleFunc("GET /admin/logout", a.handleAdminLogout)
	lan.HandleFunc("GET /admin", a.handleAdmin)
	lan.HandleFunc("POST /admin/services", a.handleAdminService)
	lan.HandleFunc("POST /admin/users", a.handleAdminUser)
	lan.HandleFunc("POST /admin/access", a.handleAdminAccess)
	a.lanMux = lan

	return a, nil
}

// ServiceCount is how many services are registered right now. After the first
// run that is the state file's list, not the config file's, which is why the
// startup log asks the app rather than counting cfg.Services.
func (a *App) ServiceCount() int { return len(*a.services.Load()) }

// hstsValue is attached to every response served over the public listener. Six
// months is long enough to matter and short enough to walk back after a
// mistake; includeSubDomains matches the wildcard tunnel and the zone-wide
// session cookie.
const hstsValue = "max-age=15552000; includeSubDomains"

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := netutil.NormalizeHost(r.Host)

	// Harmless over plain http (browsers ignore HSTS there) and the only way to
	// pin https for the responses that do arrive over TLS.
	if a.cfg.Server.PublicScheme == "https" {
		w.Header().Set("Strict-Transport-Security", hstsValue)
	}

	if host == a.cfg.Server.PortalHost {
		if a.redirectToHTTPS(w, r, host) {
			return
		}
		a.portalMux.ServeHTTP(w, r)
		return
	}

	entry, ok := (*a.services.Load())[host]
	if !ok {
		// An unknown host gets a plain 404 rather than a redirect to the
		// portal, which would confirm which subdomains exist to anyone
		// probing.
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}

	// Checked only for hosts that exist, so the redirect cannot be used to
	// reflect an arbitrary Host header back at the caller.
	if a.redirectToHTTPS(w, r, host) {
		return
	}

	// Authentication covers the service subdomains, not just the portal. A
	// request to router-vpn.example.com must not reach the router without
	// a session.
	user, ok := a.auth.Authenticate(w, r)
	if !ok {
		return
	}

	// Authorization, which is the part that did not exist before. Being logged
	// in is no longer the same as being allowed everywhere, and this is checked
	// per request against the current state, so revoking someone takes effect on
	// their very next request rather than whenever their token expires.
	if !a.store.Allows(user, entry.svc.Subdomain) {
		a.logger.Warn("access denied",
			"user", user, "service", entry.svc.Subdomain, "path", r.URL.Path)
		http.Error(w, "403 forbidden", http.StatusForbidden)
		return
	}

	entry.handler.ServeHTTP(w, r)
}

// redirectToHTTPS sends the browser back over TLS when the request reached the
// edge over plain http, and reports whether it did.
//
// The scheme has to come from Cloudflare's headers, not r.TLS: cloudflared
// always speaks plain http to this listener, so r.TLS is nil even for a page
// the browser loaded over https. A 308 rather than a 301 because a login POST
// must keep its method and body on the retry. HSTS is the real defence against
// this ever happening after the first visit; this is the part that works even
// before the browser has seen that header.
func (a *App) redirectToHTTPS(w http.ResponseWriter, r *http.Request, host string) bool {
	if a.cfg.Server.PublicScheme != "https" || netutil.ClientScheme(r) == "https" {
		return false
	}
	// WebSocket clients do not replay a failed handshake at the redirect target;
	// they just report an error. A page that is still being served over plain
	// http would lose its socket entirely, so leave upgrades to the proxy. The
	// page navigation that opened the socket is redirected as usual.
	if r.Header.Get("Upgrade") != "" {
		return false
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	return true
}

// rebuild recreates the host-to-handler map from the current state.
//
// Handlers are rebuilt rather than patched because proxy.New is cheap and has
// no global state; the only thing worth reclaiming is the connection pool of a
// handler that is being dropped.
func (a *App) rebuild() {
	snap := a.store.Snapshot()
	targets := snap.Targets()

	next := make(map[string]serviceEntry, len(snap.Services))
	for i := range snap.Services {
		svc := &snap.Services[i]
		host := svc.Subdomain + "." + a.cfg.Server.BaseDomain
		next[host] = serviceEntry{
			svc: svc,
			handler: proxy.New(svc, targets[svc.Subdomain], proxy.Options{
				SessionCookieName: a.cfg.Session.CookieName,
				AllowLoopback:     a.cfg.Server.AllowLoopbackTargets,
				Logger:            a.logger.With("service", svc.Name),
			}),
		}
		a.logger.Debug("service registered", "host", host, "target", svc.Target)
	}

	if old := a.services.Swap(&next); old != nil {
		for _, e := range *old {
			e.handler.CloseIdleConnections()
		}
	}
}

func (a *App) handlePortal(w http.ResponseWriter, r *http.Request) {
	user, ok := a.auth.Authenticate(w, r)
	if !ok {
		return
	}

	a.views.RenderPortal(w, http.StatusOK, portal.PortalData{
		User:     user,
		Services: a.serviceCards(user, false),
	})
}

// serviceCards builds the card list for both copies of the index. The
// differences are which address a card points at -- the public subdomain, or
// the service's own address on the LAN -- and whether access is filtered.
func (a *App) serviceCards(user string, lan bool) []portal.Service {
	snap := a.store.Snapshot()
	targets := snap.Targets()

	cards := make([]portal.Service, 0, len(snap.Services))
	for i := range snap.Services {
		svc := &snap.Services[i]

		// The LAN index is unauthenticated and deliberately unfiltered. It is
		// served on a network where these backends are reachable directly
		// anyway, so omitting an entry would cost the page its whole point
		// without withholding anything: the access rules govern the tunnel, and
		// only the tunnel.
		if !lan && !a.store.Allows(user, svc.Subdomain) {
			continue
		}

		link := a.serviceURL(svc.Subdomain)
		if lan {
			link = a.cfg.LANServiceURL(svc, targets[svc.Subdomain])
		}
		cards = append(cards, portal.Service{
			Name:        svc.Name,
			Description: svc.Description,
			URL:         link,
			Note:        svc.Note,
		})
	}
	return cards
}

func (a *App) serviceURL(subdomain string) string {
	return a.cfg.Server.PublicScheme + "://" + subdomain + "." + a.cfg.Server.BaseDomain + "/"
}
