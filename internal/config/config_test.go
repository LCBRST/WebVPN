package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validDoc = `
server:
  listen: "127.0.0.1:8080"
  base_domain: "example.com"
  portal_host: "vpn.example.com"
  public_scheme: "https"
session:
  signing_key: "0123456789abcdef0123456789abcdef"
users:
  - name: "admin"
    password_hash: "$2a$12$abcdefghijklmnopqrstuvwx"
services:
  - name: "App"
    subdomain: "app-vpn"
    target: "http://192.168.1.1"
`

func load(t *testing.T, doc string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func mustLoad(t *testing.T, doc string) *Config {
	t.Helper()
	cfg, err := load(t, doc)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// The per-service switches are tri-state, and the defaults() trick does not
// reach inside a slice. This is the regression test for that: an early version
// silently defaulted all three to off, which meant the session cookie was
// forwarded to every backend.
func TestServiceSwitchesDefaultOn(t *testing.T) {
	svc := &mustLoad(t, validDoc).Services[0]

	if !svc.ShouldSendXFF() {
		t.Error("send_xff should default to on")
	}
	if !svc.ShouldStripSessionCookie() {
		t.Error("strip_session_cookie should default to on")
	}
	if !svc.ShouldRewriteLocation() {
		t.Error("rewrite_location should default to on")
	}
}

func TestServiceSwitchesCanBeTurnedOff(t *testing.T) {
	svc := &mustLoad(t, validDoc+`    send_xff: false
    strip_session_cookie: false
    rewrite_location: false
`).Services[0]

	if svc.ShouldSendXFF() {
		t.Error("send_xff: false was ignored")
	}
	if svc.ShouldStripSessionCookie() {
		t.Error("strip_session_cookie: false was ignored")
	}
	if svc.ShouldRewriteLocation() {
		t.Error("rewrite_location: false was ignored")
	}
}

func TestValidateServicesReturnsParsedTargets(t *testing.T) {
	cfg := mustLoad(t, validDoc)
	got, err := cfg.ValidateServices(cfg.Services)
	if err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
	if u := got["app-vpn"]; u == nil || u.String() != "http://192.168.1.1" {
		t.Errorf("target for app-vpn = %v, want http://192.168.1.1", u)
	}
}

// lanURL resolves the parsed target the way the store does and asks for the
// LAN-facing address of service i. The helpers under test take the target
// explicitly, because the live service list is mutable and a lookup inside the
// config would miss anything added since startup.
func lanURL(t *testing.T, cfg *Config, i int) string {
	t.Helper()
	targets, err := cfg.ValidateServices(cfg.Services)
	if err != nil {
		t.Fatalf("ValidateServices: %v", err)
	}
	svc := &cfg.Services[i]
	return cfg.LANServiceURL(svc, targets[svc.Subdomain])
}

func TestLANIndexIsOffByDefault(t *testing.T) {
	if mustLoad(t, validDoc).LANIndexEnabled() {
		t.Error("lan_index must stay off unless a listen address is configured")
	}
}

// The common case: the service already listens on an address the LAN can reach,
// so the card is just the target.
func TestLANIndexUsesTheTargetAsWritten(t *testing.T) {
	cfg := mustLoad(t, validDoc+`
lan_index:
  listen: "192.168.1.5:80"
`)
	if !cfg.LANIndexEnabled() {
		t.Fatal("lan_index should be on")
	}
	if got := lanURL(t, cfg, 0); got != "http://192.168.1.1" {
		t.Errorf("LANServiceURL = %q, want the target unchanged", got)
	}
}

// A browser on a phone resolves 127.0.0.1 to the phone, so a loopback target
// has to be rewritten or every such card is a dead link.
func TestLANIndexRewritesLoopbackTargets(t *testing.T) {
	doc := strings.Replace(validDoc, "http://192.168.1.1", "http://127.0.0.1:9191", 1) + `
lan_index:
  listen: "192.168.1.5:80"
  host: "192.168.1.5"
`
	cfg := mustLoad(t, doc)
	if got := lanURL(t, cfg, 0); got != "http://192.168.1.5:9191" {
		t.Errorf("LANServiceURL = %q, want http://192.168.1.5:9191 (host swapped, port kept)", got)
	}
}

func TestLANServiceURLOverrideWins(t *testing.T) {
	doc := validDoc + `    lan_url: "http://192.168.1.9:9000/"
lan_index:
  listen: "192.168.1.5:80"
`
	cfg := mustLoad(t, doc)
	if got := lanURL(t, cfg, 0); got != "http://192.168.1.9:9000/" {
		t.Errorf("LANServiceURL = %q, want the override", got)
	}
}

func TestRejectedConfigs(t *testing.T) {
	tests := []struct {
		name string
		doc  string
	}{
		{
			name: "public target address",
			doc:  strings.Replace(validDoc, "http://192.168.1.1", "http://8.8.8.8", 1),
		},
		{
			name: "target with a path",
			doc:  strings.Replace(validDoc, "http://192.168.1.1", "http://192.168.1.1/admin", 1),
		},
		{
			name: "target pointing at our own listener",
			doc:  strings.Replace(validDoc, "http://192.168.1.1", "http://127.0.0.1:8080", 1),
		},
		{
			name: "non-http target scheme",
			doc:  strings.Replace(validDoc, "http://192.168.1.1", "ftp://192.168.1.1", 1),
		},
		{
			name: "portal host outside the base domain",
			doc:  strings.Replace(validDoc, `portal_host: "vpn.example.com"`, `portal_host: "vpn.other.com"`, 1),
		},
		{
			name: "misspelled key",
			doc:  validDoc + "\nbogus_key: 1\n",
		},
		{
			name: "short signing key",
			doc:  strings.Replace(validDoc, `"0123456789abcdef0123456789abcdef"`, `"tooshort"`, 1),
		},
		{
			name: "password hash that is not bcrypt",
			doc:  strings.Replace(validDoc, "$2a$12$abcdefghijklmnopqrstuvwx", "plaintext", 1),
		},
		{
			name: "duplicate subdomain",
			doc: validDoc + `  - name: "Another"
    subdomain: "app-vpn"
    target: "http://192.168.1.2"
`,
		},
		{
			name: "subdomain colliding with the portal host",
			doc:  strings.Replace(validDoc, `subdomain: "app-vpn"`, `subdomain: "vpn"`, 1),
		},
		{
			name: "subdomain that is not a DNS label",
			doc:  strings.Replace(validDoc, `subdomain: "app-vpn"`, `subdomain: "App_VPN"`, 1),
		},
		{
			name: "lan_index listen that is not host:port",
			doc:  validDoc + "lan_index:\n  listen: \"nonsense\"\n",
		},
		{
			name: "lan_index listening on the same address as server.listen",
			doc:  validDoc + "lan_index:\n  listen: \"127.0.0.1:8080\"\n",
		},
		{
			// Nothing in the config is wrong except that the page would be a
			// list of links pointing at the reader's own machine.
			name: "loopback target with no lan_index.host to rewrite it to",
			doc: strings.Replace(validDoc, "http://192.168.1.1", "http://127.0.0.1:9191", 1) +
				"lan_index:\n  listen: \"192.168.1.5:80\"\n",
		},
		{
			name: "lan_index host carrying its own port",
			doc:  validDoc + "lan_index:\n  listen: \"192.168.1.5:80\"\n  host: \"192.168.1.5:80\"\n",
		},
		{
			name: "zero failure threshold",
			doc:  validDoc + "login:\n  max_failures: 0\n",
		},
		{
			name: "max_lockout below lockout",
			doc:  validDoc + "login:\n  lockout: 1h\n  max_lockout: 10m\n",
		},
		{
			name: "negative failure window",
			doc:  validDoc + "login:\n  failure_window: -1m\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := load(t, tt.doc); err == nil {
				t.Error("config was accepted, want an error")
			}
		})
	}
}

func TestRefreshAfterMustBeLessThanTTL(t *testing.T) {
	doc := strings.Replace(validDoc, `  signing_key: "0123456789abcdef0123456789abcdef"`,
		`  signing_key: "0123456789abcdef0123456789abcdef"
  ttl: 1h
  refresh_after: 2h`, 1)

	if _, err := load(t, doc); err == nil {
		t.Error("refresh_after greater than ttl should be rejected")
	}
}

func TestLoginLockoutDefaults(t *testing.T) {
	cfg := mustLoad(t, validDoc)

	if cfg.Login.MaxFailures != 5 {
		t.Errorf("max_failures = %d, want 5", cfg.Login.MaxFailures)
	}
	if got := cfg.Login.FailureWindow.Std(); got != 15*time.Minute {
		t.Errorf("failure_window = %v, want 15m", got)
	}
	if got := cfg.Login.Lockout.Std(); got != 15*time.Minute {
		t.Errorf("lockout = %v, want 15m", got)
	}
	if got := cfg.Login.MaxLockout.Std(); got != 4*time.Hour {
		t.Errorf("max_lockout = %v, want 4h", got)
	}
}
