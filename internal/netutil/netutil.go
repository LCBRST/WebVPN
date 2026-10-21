// Package netutil holds small helpers for reading the host, client IP and
// scheme of requests that arrive through a Cloudflare Tunnel.
//
// Everything here assumes the listener is bound to loopback and that the only
// process that can reach it is cloudflared running on the same machine. That
// assumption is what makes it safe to trust CF-Connecting-IP and friends; if
// the listener ever moves to a LAN address these helpers become spoofable.
package netutil

import (
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// NormalizeHost lowercases a Host header, drops any port, and strips the
// trailing dot some clients send for an absolute FQDN. Routing keys are built
// from this, so skipping any of the three steps makes lookups miss for no
// visible reason.
func NormalizeHost(h string) string {
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// StripPort is NormalizeHost without the lowercasing, for comparing against
// host names read out of the config file.
func StripPort(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.TrimSuffix(h, ".")
}

// UnderDomain reports whether host is domain itself or a subdomain of it.
func UnderDomain(host, domain string) bool {
	host, domain = NormalizeHost(host), NormalizeHost(domain)
	if host == "" || domain == "" {
		return false
	}
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// Label returns the subdomain part of host relative to domain: "router" for
// ("router.example.com", "example.com"). It returns "" when host is the domain
// itself or is not under it at all.
func Label(host, domain string) string {
	host, domain = NormalizeHost(host), NormalizeHost(domain)
	if !UnderDomain(host, domain) || host == domain {
		return ""
	}
	return strings.TrimSuffix(host, "."+domain)
}

// ClientIP returns the address of the browser.
//
// RemoteAddr is useless here: cloudflared dials the origin over loopback, so it
// is always 127.0.0.1. CF-Connecting-IP is written by Cloudflare's edge and is
// the only value in the chain a client cannot forge.
//
// The forwarding headers are trusted only when the peer is loopback. Production
// always is -- the listener binds 127.0.0.1 -- but if it were ever exposed more
// widely, trusting the header from a LAN peer would let that peer choose its own
// rate-limit bucket (and log line) by sending any value it likes.
func ClientIP(r *http.Request) string {
	if isLoopbackRemote(r.RemoteAddr) {
		if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Cloudflare appends, so the rightmost entry is the most recent hop.
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	return remoteHost(r.RemoteAddr)
}

func isLoopbackRemote(remoteAddr string) bool {
	host := remoteHost(remoteAddr)
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// RateKey is the identity a rate limiter should count against.
//
// It canonicalises the address so that spellings of the same client -- an
// IPv4-mapped IPv6 form, or a rotating address inside one IPv6 /64 -- land in
// one bucket. IPv4 stays per address; IPv6 is grouped by the /64 that a single
// connection is normally assigned. Without this, an IPv6 client could sidestep
// a lockout by changing its last 64 bits.
func RateKey(ip string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return addr.String()
	}
	return prefix.String()
}

// ClientScheme returns the scheme the browser used, which is not the scheme of
// the request we are holding: cloudflared always speaks plain HTTP to the
// origin, so r.TLS is nil even for a page loaded over https://. Deriving it
// correctly matters because backends build absolute redirects from it, and an
// http:// redirect from an https:// page is a broken link.
func ClientScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		return strings.ToLower(p)
	}
	if v := r.Header.Get("CF-Visitor"); v != "" {
		var visitor struct {
			Scheme string `json:"scheme"`
		}
		if json.Unmarshal([]byte(v), &visitor) == nil && visitor.Scheme != "" {
			return strings.ToLower(visitor.Scheme)
		}
	}
	return "http"
}
