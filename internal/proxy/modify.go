package proxy

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/LCBRST/webvpn/internal/config"
	"github.com/LCBRST/webvpn/internal/netutil"
)

// newModifier builds the ModifyResponse hook, which is where the response is
// beaten into something a browser can actually use.
func newModifier(svc *config.Service, target *url.URL) func(*http.Response) error {
	return func(res *http.Response) error {
		rewriteSetCookie(res.Header)

		if svc.ShouldRewriteLocation() {
			rewriteLocation(res.Header, svc, target)
			rewriteRefresh(res.Header, svc, target)
		}
		if svc.FixCORS {
			fixCORS(res, target)
		}

		// Cloudflare sits in front of all of this and will happily serve a
		// cached copy of a logged-in page to somebody else. A Bypass cache
		// rule handles most of it, this is the belt to that pair of braces.
		if strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
			res.Header.Set("Cache-Control", "private, no-store")
		}
		// Advertises the backend's HTTP/3 endpoint, which nobody can reach.
		res.Header.Del("Alt-Svc")
		res.Header.Del("Public-Key-Pins")
		return nil
	}
}

// rewriteSetCookie drops the Domain attribute from every cookie the backend
// sets.
//
// Appliances routinely stamp their own address into Domain, and a browser
// silently discards the entire cookie when Domain is not a parent of the page
// it arrived on. Clearing it leaves a host-only cookie, which the browser binds
// to the public subdomain instead.
func rewriteSetCookie(h http.Header) {
	lines := h.Values("Set-Cookie")
	if len(lines) == 0 {
		return
	}
	h.Del("Set-Cookie")

	for _, line := range lines {
		// Parse one header line at a time. Splitting the joined header on ","
		// is a classic bug because Expires contains a comma:
		//   Expires=Wed, 21 Oct 2015 07:28:00 GMT
		c, err := http.ParseSetCookie(line)
		if err != nil {
			// A cookie we cannot parse is still the backend's business; put it
			// back untouched rather than dropping it on the floor.
			h.Add("Set-Cookie", line)
			continue
		}

		c.Domain = ""

		// __Host- and __Secure- are browser-enforced contracts. Break one and
		// the cookie disappears with no error in any log.
		switch {
		case strings.HasPrefix(c.Name, "__Host-"):
			c.Secure, c.Path = true, "/"
		case strings.HasPrefix(c.Name, "__Secure-"):
			c.Secure = true
		}
		// SameSite=None is only honoured on a Secure cookie.
		if c.SameSite == http.SameSiteNoneMode {
			c.Secure = true
		}

		h.Add("Set-Cookie", c.String())
	}
}

func rewriteLocation(h http.Header, svc *config.Service, target *url.URL) {
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", toPublicRef(loc, svc, target))
	}
}

var refreshRE = regexp.MustCompile(`(?i)^(\s*[\d.]+\s*;\s*url\s*=\s*)(.*)$`)

// rewriteRefresh handles the Refresh header, which older routers use in place
// of Location.
func rewriteRefresh(h http.Header, svc *config.Service, target *url.URL) {
	m := refreshRE.FindStringSubmatch(h.Get("Refresh"))
	if m == nil {
		return
	}
	dest := strings.Trim(strings.TrimSpace(m[2]), `"'`)
	h.Set("Refresh", m[1]+toPublicRef(dest, svc, target))
}

// toPublicRef rewrites an absolute URL that points at the backend into a
// relative reference.
//
// Without this, a backend that answers with "Location: http://192.168.1.1/login"
// sends the browser straight at an address it cannot reach from outside (broken
// page), or straight at the appliance, bypassing the login this proxy exists to
// enforce (worse).
//
// A relative reference is used rather than rebuilding an absolute public URL
// because it needs no knowledge of our own host name and cannot be wrong:
// subdomain mode keeps paths 1:1 with the backend.
func toPublicRef(loc string, svc *config.Service, target *url.URL) string {
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return loc // already relative, or nothing we can reason about
	}
	if !isBackendHost(u.Host, svc, target) {
		return loc
	}

	ref := u.EscapedPath()
	if ref == "" {
		ref = "/"
	}
	if u.RawQuery != "" {
		ref += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		ref += "#" + u.Fragment
	}
	return ref
}

// isBackendHost reports whether host refers to the backend itself. The extra
// list covers appliances that redirect to a name of their own: a router that
// jumps to router.lan, Grafana to localhost:3000, a NAS to its FQDN.
func isBackendHost(host string, svc *config.Service, target *url.URL) bool {
	got := strings.ToLower(netutil.StripPort(host))
	if got == strings.ToLower(target.Hostname()) {
		return true
	}
	for _, extra := range svc.ExtraLocationHosts {
		if strings.ToLower(netutil.StripPort(extra)) == got {
			return true
		}
	}
	return false
}

// fixCORS rewrites an Access-Control-Allow-Origin that names the backend's own
// address, which a browser would reject as a cross-origin mismatch.
//
// Never substitutes "*" when credentials are allowed; that combination is
// invalid and would be rejected, so the request's Origin is echoed instead.
func fixCORS(res *http.Response, target *url.URL) {
	h := res.Header
	acao := h.Get("Access-Control-Allow-Origin")
	if acao == "" || acao == "*" {
		return
	}
	u, err := url.Parse(acao)
	if err != nil || u.Host == "" {
		return
	}
	if !strings.EqualFold(netutil.StripPort(u.Host), target.Hostname()) {
		return
	}
	// res.Request is the outbound request; its Origin header is the one the
	// browser actually sent.
	origin := res.Request.Header.Get("Origin")
	if origin == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
}
