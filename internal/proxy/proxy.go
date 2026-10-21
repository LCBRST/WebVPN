// Package proxy reverse-proxies one internal service onto one public
// subdomain.
//
// Nothing here rewrites response bodies. Subdomain routing keeps paths 1:1 with
// the backend, so relative URLs, absolute paths in scripts, and WebSocket
// endpoints all resolve correctly on their own. Only the handful of things that
// carry the backend's own name — cookies, redirects — need fixing up, and both
// live in modify.go.
package proxy

import (
	"crypto/tls"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/LCBRST/webvpn/internal/config"
)

// Options carries the settings that come from the server section of the config
// rather than from an individual service.
type Options struct {
	// SessionCookieName is removed from requests before they reach a backend.
	SessionCookieName string
	// AllowLoopback permits targets on 127.0.0.0/8, which is needed whenever a
	// service runs on the same machine as this proxy.
	AllowLoopback bool
	Logger        *slog.Logger
}

// Handler proxies one service.
type Handler struct {
	svc *config.Service
	rp  *httputil.ReverseProxy
}

func New(svc *config.Service, target *url.URL, opts Options) *Handler {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	rp := &httputil.ReverseProxy{
		Rewrite:        newRewrite(svc, target, opts.SessionCookieName),
		Transport:      newTransport(svc, opts.AllowLoopback),
		ModifyResponse: newModifier(svc, target),
		// Set explicitly rather than relying on the default. Without it, a
		// server-sent event feed buffers up and arrives in one lump when the
		// connection ends, which looks like the page hanging.
		FlushInterval: 100 * time.Millisecond,
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The default handler writes a bare 502 with no clue about which
			// backend failed.
			logger.Error("upstream request failed",
				"service", svc.Name, "target", svc.Target, "err", err)
			writeErrorPage(w, "内网服务「"+svc.Name+"」当前不可达", svc.Target, err)
		},
	}
	return &Handler{svc: svc, rp: rp}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.rp.ServeHTTP(w, r)
}

// CloseIdleConnections drops this handler's pooled connections to its backend.
//
// Called when the service list is rebuilt after an edit, so the pools of the
// handlers being thrown away do not sit there holding sockets until their idle
// timeout. Only idle connections go: a request in flight, including a WebSocket
// or an event stream, keeps its own connection and finishes normally.
func (h *Handler) CloseIdleConnections() {
	// Comma-ok rather than a bare assertion: this runs while an admin edit is
	// being applied, and panicking there would take the process down over
	// connection-pool housekeeping.
	if t, ok := h.rp.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

func newTransport(svc *config.Service, allowLoopback bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		// Runs after DNS resolution, so it also catches a host name that
		// resolves somewhere it should not.
		Control: Guard{AllowLoopback: allowLoopback}.Control,
	}

	return &http.Transport{
		DialContext: dialer.DialContext,

		// Must stay true. Left at the default, the transport adds
		// Accept-Encoding: gzip itself and then transparently decompresses,
		// stripping Content-Encoding and Content-Length and mangling range
		// requests. Bytes the backend already compressed end up decoded and
		// re-sent, for no benefit and some confusion.
		DisableCompression: true,

		// Keep HTTP/1.1 towards the backend: WebSocket upgrades do not survive
		// HTTP/2. Setting this explicitly guards against a future "performance
		// improvement" quietly turning it back on.
		ForceAttemptHTTP2: false,

		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 16, // the default of 2 churns connections badly
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,

		TLSClientConfig: &tls.Config{
			//nolint:gosec // appliance certificates are self-signed by design
			InsecureSkipVerify: svc.InsecureTLS,
			ServerName:         svc.TLSServerName,
			MinVersion:         tls.VersionTLS12,
		},
	}
}

// writeErrorPage renders a self-contained page. It deliberately does not use
// the portal templates: an error page that needs template parsing is an error
// page that can fail.
func writeErrorPage(w http.ResponseWriter, title, detail string, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)

	fmt.Fprintf(w, `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:grid; place-items:center;
         font:15px/1.6 system-ui,-apple-system,"Segoe UI",sans-serif; }
  .box { max-width:32rem; padding:2rem; }
  h1 { font-size:1.25rem; margin:0 0 .5rem; }
  .detail { margin:0 0 1.25rem; color:#b3261e; }
  dl { margin:0; padding:1rem; background:#8881; border-radius:.5rem;
       font-size:.8125rem; }
  dt { font-weight:600; opacity:.7; }
  dd { margin:.15rem 0 .75rem; font-family:ui-monospace,Consolas,monospace;
       word-break:break-all; }
  dd:last-child { margin-bottom:0; }
</style>
</head>
<body>
  <div class="box">
    <h1>%s</h1>
    <p class="detail">%s</p>
    <dl>
      <dt>后端地址</dt><dd>%s</dd>
      <dt>错误</dt><dd>%s</dd>
    </dl>
  </div>
</body>
</html>
`,
		html.EscapeString(title),
		html.EscapeString(title),
		html.EscapeString(detail),
		html.EscapeString(detail),
		html.EscapeString(err.Error()),
	)
}
