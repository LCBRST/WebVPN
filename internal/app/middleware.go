package app

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/LCBRST/webvpn/internal/netutil"
)

// Middleware adds panic recovery, access logging and baseline security headers.
func Middleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if v := recover(); v != nil {
				logger.Error("panic serving request",
					"host", r.Host, "method", r.Method, "path", r.URL.Path,
					"panic", v, "stack", string(debug.Stack()))
				if !rec.wrote {
					http.Error(rec, "internal error", http.StatusInternalServerError)
				}
			}
		}()

		header := rec.Header()
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")
		// No HSTS here on purpose. It would be set for the whole zone from a
		// single host, and a long max-age with includeSubDomains is painful to
		// undo.

		next.ServeHTTP(rec, r)

		// Query strings are left out deliberately: plenty of appliance admin
		// panels carry tokens in them, and a log file is a bad place for those.
		// No headers are logged either, for the same reason.
		logger.Info("request",
			"host", netutil.NormalizeHost(r.Host),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"dur", time.Since(start).Round(time.Millisecond),
			"ip", netutil.ClientIP(r),
		)
	})
}

// responseRecorder remembers the status code so the access log can report it.
//
// It must forward Hijack, Flush and Unwrap. Embedding an http.ResponseWriter in
// a struct hides any interface the original also implemented, so a naive
// wrapper silently removes http.Hijacker — and WebSocket upgrades, which need
// to take over the raw connection, then fail with "response does not implement
// http.Hijacker" while ordinary requests keep working perfectly. Unwrap is what
// http.ResponseController follows on newer Go versions.
type responseRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *responseRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}
