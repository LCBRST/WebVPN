package proxy

import "testing"

func TestGuardBlocksAnythingNotPrivate(t *testing.T) {
	g := Guard{AllowLoopback: false}

	blocked := []string{
		"8.8.8.8:80",
		"1.1.1.1:443",
		"0.0.0.0:80",          // unspecified
		"169.254.169.254:80",  // cloud metadata, via link-local
		"224.0.0.1:80",        // multicast
		"[::ffff:8.8.8.8]:80", // IPv4-mapped must not slip past the v4 checks
		"[2001:4860:4860::8888]:443",
		"127.0.0.1:80", // loopback is refused unless opted in
		"[::1]:80",
	}

	for _, addr := range blocked {
		if err := g.Control("tcp", addr, nil); err == nil {
			t.Errorf("Control(%q) allowed the address", addr)
		}
	}
}

func TestGuardAllowsPrivateAddresses(t *testing.T) {
	g := Guard{AllowLoopback: false}

	allowed := []string{
		"192.168.1.1:80",
		"10.0.0.5:8080",
		"172.16.0.1:80",
		"172.31.255.254:80",
		"[fd00::1]:80",
	}

	for _, addr := range allowed {
		if err := g.Control("tcp", addr, nil); err != nil {
			t.Errorf("Control(%q) = %v, want nil", addr, err)
		}
	}
}

func TestGuardLoopbackOptIn(t *testing.T) {
	g := Guard{AllowLoopback: true}

	if err := g.Control("tcp", "127.0.0.1:3000", nil); err != nil {
		t.Errorf("loopback should be allowed when opted in: %v", err)
	}
	if err := g.Control("tcp", "[::1]:3000", nil); err != nil {
		t.Errorf("IPv6 loopback should be allowed when opted in: %v", err)
	}
	// Opting into loopback must not open anything else up.
	if err := g.Control("tcp", "8.8.8.8:80", nil); err == nil {
		t.Error("opting into loopback also allowed a public address")
	}
}

func TestGuardRejectsNonTCPNetworks(t *testing.T) {
	g := Guard{AllowLoopback: true}

	if err := g.Control("udp", "192.168.1.1:53", nil); err == nil {
		t.Error("a non-TCP network was allowed")
	}
}
