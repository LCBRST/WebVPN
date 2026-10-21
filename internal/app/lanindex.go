package app

import (
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/LCBRST/webvpn/internal/portal"
)

// LANHandler is what the LAN listener serves: the free index at the root, and
// the admin page under /admin.
//
// The mux is built once in New. This handler is still not reachable through
// ServeHTTP -- it is mounted on its own listener -- so it cannot be talked into
// proxying anything by way of a Host header.
func (a *App) LANHandler() http.Handler { return a.lanMux }

// handleLANIndex renders the unauthenticated index of services.
func (a *App) handleLANIndex(w http.ResponseWriter, r *http.Request) {
	a.views.RenderPortal(w, http.StatusOK, portal.PortalData{
		LAN:      true,
		Services: a.serviceCards("", true),
	})
}

// LANMiddleware is Middleware's counterpart for the LAN listener, and it is a
// separate function on purpose.
//
// Middleware logs netutil.ClientIP, which trusts CF-Connecting-IP and
// X-Forwarded-For. That is sound on the tunnel listener, where cloudflared is
// the only process that can reach loopback, but here every client on the LAN
// can set those headers and the log would report whatever they felt like. This
// one logs the address the kernel hands us.
func LANMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}

		// Redundant while the listener is bound to a LAN address, and that is
		// the point: it is the difference between "someone edited listen to
		// 0.0.0.0" and "the service list, and now the admin page, are on the
		// internet".
		if !privateAddr(host) {
			logger.Warn("refused a client from outside the LAN", "ip", host, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic serving request",
					"method", r.Method, "path", r.URL.Path, "panic", v, "stack", string(debug.Stack()))
				if !rec.wrote {
					http.Error(rec, "internal error", http.StatusInternalServerError)
				}
			}
		}()

		header := rec.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")

		next.ServeHTTP(rec, r)

		// The index carries no query string to redact, so unlike Middleware
		// this one could log it. It does not, to keep the two access logs the
		// same shape.
		logger.Info("request",
			"listener", "lan",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur", time.Since(start).Round(time.Millisecond),
			"ip", host,
		)
	})
}

// privateAddr reports whether host is a private or loopback address.
//
// Loopback counts because the machine itself is a legitimate client when you
// are checking the page over ssh; anything else is the world.
func privateAddr(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// Fold ::ffff:192.168.1.1 down first, or the range checks look at a 128-bit
	// address whose IPv4 half is the part that matters.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate()
}
