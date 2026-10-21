package proxy

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrBlockedTarget is returned when a backend address fails the allowlist.
var ErrBlockedTarget = errors.New("target address not allowed")

// Guard is a net.Dialer.Control hook that refuses to connect anywhere except
// private address space.
//
// Checking the config alone is not enough. The config can only see the literal
// text of the target, so a host name that resolves into public space, or a name
// that resolves privately once during validation and publicly when the dial
// happens (DNS rebinding), would sail straight through. Control runs after
// resolution and before connect, so it sees the address that is actually about
// to be dialled.
type Guard struct {
	// AllowLoopback permits 127.0.0.0/8 and ::1, needed when a service runs on
	// the same machine as this proxy.
	AllowLoopback bool
}

// Control implements the net.Dialer.Control signature.
func (g Guard) Control(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return fmt.Errorf("%w: network %q", ErrBlockedTarget, network)
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBlockedTarget, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: unparsable address %q", ErrBlockedTarget, host)
	}
	// Fold ::ffff:192.168.1.1 down to its IPv4 form first. The range checks
	// below would otherwise look at a 128-bit address whose v4 part is what
	// actually matters, and let it through.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	switch {
	case ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// Link-local covers 169.254.169.254, the cloud metadata endpoint.
		return fmt.Errorf("%w: %s", ErrBlockedTarget, ip)
	case ip.IsLoopback():
		if !g.AllowLoopback {
			return fmt.Errorf("%w: loopback %s", ErrBlockedTarget, ip)
		}
	case !ip.IsPrivate():
		// Allowlist, not blocklist: anything that is not private is refused,
		// which also covers the ranges net.IP.IsPrivate does not know about
		// such as 100.64.0.0/10 (CGNAT) and 198.18.0.0/15.
		return fmt.Errorf("%w: %s is not a private address", ErrBlockedTarget, ip)
	}
	return nil
}
