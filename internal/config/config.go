// Package config loads and validates the YAML configuration.
//
// Every problem is reported at startup rather than at request time: a target on
// a public address or a duplicated subdomain is a config bug you want to see
// while you still have a shell open, not the first time you are on a phone
// abroad trying to reach your router.
package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/LCBRST/webvpn/internal/netutil"
)

// Duration lets the YAML say "10s" instead of a nanosecond count.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("expected a duration string such as \"10s\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Server   Server    `yaml:"server"`
	Session  Session   `yaml:"session"`
	Users    []User    `yaml:"users"`
	Login    Login     `yaml:"login"`
	Services []Service `yaml:"services"`
	LANIndex LANIndex  `yaml:"lan_index"`
	State    State     `yaml:"state"`

	// Filled in by finish().
	//
	// Parsed targets are deliberately not among them. Services can be added and
	// removed while the process runs, so the live set lives in internal/store
	// and is parsed by ValidateServices, which the store calls on every change.
	// A copy here would be a second, staler answer to the same question.
	signingKey []byte
}

// State is where the mutable half of the configuration lives.
//
// Services and users are edited from the admin page while the process runs, so
// they cannot stay in a file that is read once at startup. They are seeded from
// Users and Services above the first time the state file does not yet exist;
// from then on the seed is ignored and the state file is authoritative.
// config.yaml's users: and services: become a record of what was true on the
// day the admin page was introduced, not a thing that has any effect.
type State struct {
	// Path is the JSON state file. Empty means "state.json next to the config
	// file", which is fine for a local test and wrong in production, where
	// deploy/webvpn.service makes the config directory read-only.
	//
	// That unit sets StateDirectory=webvpn, which creates /var/lib/webvpn owned
	// by the service user, which is the place this is meant to live.
	Path string `yaml:"path"`
}

// SessionKey is the decoded session signing key.
func (c *Config) SessionKey() []byte { return c.signingKey }

type Server struct {
	Listen            string   `yaml:"listen"`
	BaseDomain        string   `yaml:"base_domain"`
	PortalHost        string   `yaml:"portal_host"`
	PublicScheme      string   `yaml:"public_scheme"`
	ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
	IdleTimeout       Duration `yaml:"idle_timeout"`

	// AllowLoopbackTargets permits proxies to services on 127.0.0.0/8, which
	// is the normal case when a service runs on the same machine as this
	// proxy. On by default.
	AllowLoopbackTargets bool `yaml:"allow_loopback_targets"`
	// WriteTimeout is deliberately absent. It must stay 0 or server-sent
	// events and long-polls get cut off mid-stream, and WebSocket traffic
	// hides that bug because hijacked connections ignore it.
}

// LANIndex is a second, separate listener that serves an unauthenticated index
// of the same services, addressed directly on the LAN.
//
// It exists because reaching the tunnel from inside the house is silly: the
// phone has to leave the LAN, come back through Cloudflare and log in, when the
// service is one hop away. This listener renders one page and nothing else.
//
// The listener is off unless configured, and the address it binds is the whole
// security story: see the note on Listen below.
type LANIndex struct {
	// Listen is the host:port to bind. Empty disables the listener.
	//
	// Bind a LAN address, not 0.0.0.0. The whole point of this feature is that
	// the page stays inside the network, and on a machine with a public address
	// 0.0.0.0 publishes the list of what you run to anyone who asks.
	Listen string `yaml:"listen"`

	// Host replaces the host of a loopback target in the links on the page.
	//
	// A browser on another machine resolves 127.0.0.1 to itself, so a service
	// listening on loopback is not clickable from the LAN without this. It is
	// required as soon as any service targets loopback, and unused otherwise.
	Host string `yaml:"host"`
}

// LANIndexEnabled reports whether the second listener should be started.
func (c *Config) LANIndexEnabled() bool { return c.LANIndex.Listen != "" }

type Session struct {
	CookieName   string   `yaml:"cookie_name"`
	SigningKey   string   `yaml:"signing_key"`
	TTL          Duration `yaml:"ttl"`
	RefreshAfter Duration `yaml:"refresh_after"`
	// Epoch is embedded in every token. Bumping it and restarting invalidates
	// every session ever issued, which is the escape hatch that lets us get
	// away with stateless cookies.
	Epoch int `yaml:"epoch"`
}

// User is an account. It is also the on-disk shape in state.json, which is why
// it carries JSON tags as well: everything here round-trips through the state
// file once the admin page has been used.
type User struct {
	Name         string `yaml:"name" json:"name"`
	PasswordHash string `yaml:"password_hash" json:"password_hash"`

	// Admin grants access to the LAN admin page and, implicitly, to every
	// service. There is always at least one: the store refuses to persist a
	// state in which nobody is an admin, because that state has no way back.
	Admin bool `yaml:"admin" json:"admin,omitempty"`

	// Services is the allowlist of subdomains this user may reach. An empty
	// list means none of them -- authorization is default-deny, so a service
	// added later is not silently granted to everyone.
	//
	// It covers the tunnel only. Anything the user can reach directly on the
	// LAN stays reachable, so this is "what the internet path will carry for
	// you", not "what you can see".
	Services []string `yaml:"services" json:"services,omitempty"`
}

type Login struct {
	AttemptsPerMinute int      `yaml:"attempts_per_minute"`
	MaxConcurrent     int      `yaml:"max_concurrent"`
	MaxFailures       int      `yaml:"max_failures"`
	FailureWindow     Duration `yaml:"failure_window"`
	Lockout           Duration `yaml:"lockout"`
	MaxLockout        Duration `yaml:"max_lockout"`
}

type Service struct {
	Name        string `yaml:"name" json:"name"`
	Subdomain   string `yaml:"subdomain" json:"subdomain"`
	Target      string `yaml:"target" json:"target"`
	Description string `yaml:"description" json:"description,omitempty"`

	InsecureTLS   bool   `yaml:"insecure_tls" json:"insecure_tls,omitempty"`
	TLSServerName string `yaml:"tls_server_name" json:"tls_server_name,omitempty"`

	// PreserveHost forwards the public host name to the backend instead of the
	// target's. Most appliances validate Host and want the target's, some
	// vhost-style backends want the public one.
	PreserveHost bool `yaml:"preserve_host" json:"preserve_host,omitempty"`

	// These three are pointers so that "not mentioned in the config" can be
	// told apart from "explicitly set to false". The defaults() trick used for
	// the top-level sections does not reach inside a slice, because decoding
	// replaces the whole slice rather than merging into it, so every
	// Service field would otherwise start at Go's zero value. Read them
	// through the Should* accessors, never directly.
	//
	// SendXFF forwards the client address. Turn it off for appliances that
	// trust X-Forwarded-For for their own IP allowlist, which would otherwise
	// see a public address and reject you with a 403.
	//
	// The omitempty on the JSON side is what keeps them tri-state in
	// state.json too: absent stays nil, which resolves to the default, rather
	// than becoming an explicit false.
	SendXFF *bool `yaml:"send_xff" json:"send_xff,omitempty"`

	// StripSessionCookie keeps our session token out of requests to the
	// backend. See the note in proxy/rewrite.go for why this matters.
	StripSessionCookie *bool `yaml:"strip_session_cookie" json:"strip_session_cookie,omitempty"`

	RewriteLocation    *bool    `yaml:"rewrite_location" json:"rewrite_location,omitempty"`
	ExtraLocationHosts []string `yaml:"extra_location_hosts" json:"extra_location_hosts,omitempty"`

	// FixCORS rewrites an Access-Control-Allow-Origin response header that
	// names the backend's own address, which a browser would reject as a
	// cross-origin mismatch.
	FixCORS bool `yaml:"fix_cors" json:"fix_cors,omitempty"`

	// RewriteOrigin replaces the request's Origin header with the origin the
	// backend computes for itself. See rewrite.go for why the tunnel makes
	// those two disagree.
	RewriteOrigin bool `yaml:"rewrite_origin" json:"rewrite_origin,omitempty"`

	// LANURL is what the LAN index links to, overriding the target. Only needed
	// when the target is not reachable as written by a browser on the LAN.
	LANURL string `yaml:"lan_url" json:"lan_url,omitempty"`

	Note string `yaml:"note" json:"note,omitempty"`
}

// ShouldSendXFF, ShouldStripSessionCookie and ShouldRewriteLocation resolve the
// tri-state settings above, defaulting to on.
func (s *Service) ShouldSendXFF() bool { return resolve(s.SendXFF, true) }

func (s *Service) ShouldStripSessionCookie() bool {
	return resolve(s.StripSessionCookie, true)
}

func (s *Service) ShouldRewriteLocation() bool { return resolve(s.RewriteLocation, true) }

func resolve(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

// LANServiceURL is the address a browser on the LAN should use for s, which is
// its target except when the target is not reachable as written.
//
// target is passed in rather than looked up, because the service list is
// mutable: the caller has the parsed target from store.Snapshot, and a lookup
// in here against the startup config would miss anything added since.
func (c *Config) LANServiceURL(s *Service, target *url.URL) string {
	if s.LANURL != "" {
		return s.LANURL
	}
	u := target
	if u == nil {
		return s.Target
	}
	if c.LANIndex.Host == "" || !isLoopbackHost(u.Hostname()) {
		return u.String()
	}
	// Same scheme and port, different host. JoinHostPort rather than
	// concatenation so the port survives the swap.
	rewritten := *u
	if port := u.Port(); port != "" {
		rewritten.Host = net.JoinHostPort(c.LANIndex.Host, port)
	} else {
		rewritten.Host = c.LANIndex.Host
	}
	return rewritten.String()
}

// isLoopbackHost reports whether a target's host names this machine, which a
// browser running on any other machine would resolve to itself.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

var subdomainRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// defaults pre-populates the struct before decoding so that omitted keys keep
// these values. yaml.v3 only touches fields the document actually mentions,
// which makes this a tidy way to get defaults without a pointer per field.
func defaults() *Config {
	return &Config{
		Server: Server{
			Listen:               "127.0.0.1:8080",
			PublicScheme:         "https",
			ReadHeaderTimeout:    Duration(10 * time.Second),
			IdleTimeout:          Duration(120 * time.Second),
			AllowLoopbackTargets: true,
		},
		Session: Session{
			CookieName:   "webvpn_session",
			TTL:          Duration(7 * 24 * time.Hour),
			RefreshAfter: Duration(84 * time.Hour),
			Epoch:        1,
		},
		Login: Login{
			AttemptsPerMinute: 5,
			MaxConcurrent:     1,
			MaxFailures:       5,
			FailureWindow:     Duration(15 * time.Minute),
			Lockout:           Duration(15 * time.Minute),
			MaxLockout:        Duration(4 * time.Hour),
		},
	}
}

// Load reads, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// Catches misspelled keys instead of silently ignoring them, which saves a
	// lot of head-scratching when a service just never shows up.
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	// Before finish(), which validates it. Sitting the state file next to the
	// config file is the right default for `webvpn -config ./config.yaml` and
	// the wrong one in production: see the note on State.Path.
	if strings.TrimSpace(cfg.State.Path) == "" {
		cfg.State.Path = filepath.Join(filepath.Dir(path), "state.json")
	}
	if err := cfg.finish(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) finish() error {
	// A key in the environment is easier to keep out of backups and shell
	// history than one in the file.
	if v := os.Getenv("WEBVPN_SESSION_SECRET"); v != "" {
		c.Session.SigningKey = v
	}

	c.Server.BaseDomain = netutil.NormalizeHost(c.Server.BaseDomain)
	c.Server.PortalHost = netutil.NormalizeHost(c.Server.PortalHost)
	c.Session.CookieName = strings.TrimSpace(c.Session.CookieName)
	c.Server.PublicScheme = strings.ToLower(strings.TrimSpace(c.Server.PublicScheme))

	if err := c.validateServer(); err != nil {
		return err
	}
	if err := c.validateSession(); err != nil {
		return err
	}
	if err := c.validateLogin(); err != nil {
		return err
	}
	if err := c.ValidateUsers(c.Users); err != nil {
		return err
	}
	targets, err := c.ValidateServices(c.Services)
	if err != nil {
		return err
	}
	c.warnInsecureTLS()
	// Reads the parsed targets, so it has to come after ValidateServices.
	if err := c.validateLANIndex(targets); err != nil {
		return err
	}
	return c.validateState()
}

func (c *Config) validateState() error {
	c.State.Path = strings.TrimSpace(c.State.Path)
	if c.State.Path == "" {
		return errors.New("state.path resolved to empty; it must name a writable file")
	}
	return nil
}

// validateLANIndex takes the parsed targets rather than re-deriving them, so
// the loopback check below reads the same parse the caller just validated.
func (c *Config) validateLANIndex(targets map[string]*url.URL) error {
	c.LANIndex.Listen = strings.TrimSpace(c.LANIndex.Listen)
	c.LANIndex.Host = strings.TrimSpace(c.LANIndex.Host)

	if c.LANIndex.Listen == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(c.LANIndex.Listen); err != nil {
		return fmt.Errorf("lan_index.listen %q must be host:port", c.LANIndex.Listen)
	}
	if c.LANIndex.Listen == c.Server.Listen {
		return fmt.Errorf("lan_index.listen %q is the same address as server.listen", c.LANIndex.Listen)
	}
	// A colon here would be a port or an IPv6 literal, and both would come out
	// mangled once joined with the target's own port.
	if strings.ContainsAny(c.LANIndex.Host, ":/") {
		return fmt.Errorf("lan_index.host %q must be a bare host name or IPv4 address, without a scheme or port",
			c.LANIndex.Host)
	}

	// Refuse to start rather than serve a page of dead links: see the note on
	// LANIndex.Host.
	if c.LANIndex.Host == "" {
		for i := range c.Services {
			s := &c.Services[i]
			if u := targets[s.Subdomain]; u != nil && isLoopbackHost(u.Hostname()) {
				return fmt.Errorf("lan_index.host is required: services[%d] (%s) targets %s, "+
					"which a browser on another machine resolves to itself; "+
					"set it to this host's address on the LAN",
					i, s.Name, s.Target)
			}
		}
	}
	return nil
}

func (c *Config) validateServer() error {
	if c.Server.Listen == "" {
		return errors.New("server.listen is required")
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen %q must be host:port", c.Server.Listen)
	}
	if c.Server.BaseDomain == "" {
		return errors.New("server.base_domain is required")
	}
	if c.Server.PortalHost == "" {
		return errors.New("server.portal_host is required (wildcard tunnel rules do not match the apex)")
	}
	if !netutil.UnderDomain(c.Server.PortalHost, c.Server.BaseDomain) {
		return fmt.Errorf("server.portal_host %q must be %q or a subdomain of it",
			c.Server.PortalHost, c.Server.BaseDomain)
	}
	if c.Server.PublicScheme != "https" && c.Server.PublicScheme != "http" {
		return fmt.Errorf("server.public_scheme must be http or https, got %q", c.Server.PublicScheme)
	}
	if c.Server.PublicScheme == "http" {
		// Only sane for the local hosts-file testing loop described in the
		// plan; over the tunnel this would ship session cookies in the clear.
		fmt.Fprintln(os.Stderr, "warning: public_scheme is http, cookies will not be marked Secure")
	}
	return nil
}

func (c *Config) validateSession() error {
	if c.Session.CookieName == "" {
		return errors.New("session.cookie_name is required")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(c.Session.SigningKey))
	if err != nil {
		// Accept a raw passphrase too, it is a personal deployment.
		key = []byte(c.Session.SigningKey)
	}
	if len(key) < 16 {
		return fmt.Errorf("session.signing_key must be at least 16 bytes (got %d); run `webvpn -genkey`", len(key))
	}
	c.signingKey = key
	if c.Session.TTL.Std() <= 0 {
		return errors.New("session.ttl must be positive")
	}
	if c.Session.RefreshAfter.Std() <= 0 || c.Session.RefreshAfter.Std() >= c.Session.TTL.Std() {
		return errors.New("session.refresh_after must be positive and less than session.ttl")
	}
	return nil
}

func (c *Config) validateLogin() error {
	if c.Login.AttemptsPerMinute < 1 {
		return errors.New("login.attempts_per_minute must be at least 1")
	}
	if c.Login.MaxConcurrent < 1 {
		return errors.New("login.max_concurrent must be at least 1")
	}
	if c.Login.MaxFailures < 1 {
		return errors.New("login.max_failures must be at least 1")
	}
	if c.Login.FailureWindow.Std() <= 0 {
		return errors.New("login.failure_window must be positive")
	}
	if c.Login.Lockout.Std() <= 0 {
		return errors.New("login.lockout must be positive")
	}
	if c.Login.MaxLockout.Std() < c.Login.Lockout.Std() {
		return errors.New("login.max_lockout must be at least login.lockout")
	}
	return nil
}

// ValidateUsers checks a user list.
//
// Exported because the admin page validates the list it is about to persist
// with the same function the config file goes through, so a user it accepts is
// a user `-check` would have accepted, and the error text is the same.
func (c *Config) ValidateUsers(users []User) error {
	if len(users) == 0 {
		return errors.New("at least one entry under users is required")
	}
	seen := map[string]bool{}
	for i, u := range users {
		name := strings.TrimSpace(u.Name)
		if name == "" {
			return fmt.Errorf("users[%d].name is empty", i)
		}
		if name != u.Name {
			return fmt.Errorf("users[%d]: name %q has leading or trailing whitespace", i, u.Name)
		}
		if len(name) > 64 {
			return fmt.Errorf("users[%d]: name is longer than 64 bytes", i)
		}
		if !strings.HasPrefix(u.PasswordHash, "$2") {
			return fmt.Errorf("users[%d] (%s): password_hash is not a bcrypt hash; run `webvpn -hash`", i, u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("users: duplicate name %q", u.Name)
		}
		seen[u.Name] = true
	}
	return nil
}

// warnInsecureTLS is kept out of ValidateServices so that calling that again --
// which the admin page does on every save -- does not reprint warnings that
// have not changed since the last restart.
func (c *Config) warnInsecureTLS() {
	for i := range c.Services {
		if c.Services[i].InsecureTLS {
			fmt.Fprintf(os.Stderr,
				"warning: services[%d] (%s): insecure_tls is on, the backend certificate is not verified\n",
				i, c.Services[i].Name)
		}
	}
}

// ValidateServices checks a service list against this config's static settings
// -- base domain, portal host, our own listen address -- and returns the parsed
// targets keyed by subdomain.
//
// Exported for the same reason as ValidateUsers: the admin page runs the list
// through this before persisting it, so the subdomain regex, the collision with
// portal_host, the duplicate check, the no-path rule and the refusal to proxy a
// public address are enforced in exactly one place.
func (c *Config) ValidateServices(services []Service) (map[string]*url.URL, error) {
	// A config file with no services in it is a mistake, so it is refused.
	if len(services) == 0 {
		return nil, errors.New("at least one entry under services is required")
	}
	return c.ValidateServiceList(services)
}

// ValidateServiceList is ValidateServices without the non-empty rule, for the
// admin page, where deleting the last service is a thing someone may actually
// want to do.
//
// The two are wrappers around one body on purpose: a second copy of these rules
// is a second copy that can drift, and then the admin page would accept
// something `-check` rejects.
func (c *Config) ValidateServiceList(services []Service) (map[string]*url.URL, error) {
	targets := make(map[string]*url.URL, len(services))
	portalLabel := netutil.Label(c.Server.PortalHost, c.Server.BaseDomain)

	for i := range services {
		s := &services[i]
		where := fmt.Sprintf("services[%d] (%s)", i, s.Name)

		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("%s: name is empty", where)
		}
		if !subdomainRE.MatchString(s.Subdomain) {
			return nil, fmt.Errorf("%s: subdomain %q must be a lowercase DNS label", where, s.Subdomain)
		}
		if s.Subdomain == portalLabel {
			return nil, fmt.Errorf("%s: subdomain %q collides with portal_host", where, s.Subdomain)
		}
		if _, dup := targets[s.Subdomain]; dup {
			return nil, fmt.Errorf("%s: duplicate subdomain %q", where, s.Subdomain)
		}

		u, err := url.Parse(s.Target)
		if err != nil {
			return nil, fmt.Errorf("%s: target %q is not a URL: %w", where, s.Target, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%s: target %q must use http or https", where, s.Target)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%s: target %q has no host", where, s.Target)
		}
		if u.Path != "" && u.Path != "/" {
			// A path prefix breaks the 1:1 mapping that lets us skip rewriting
			// response bodies, so it is not supported on purpose.
			return nil, fmt.Errorf("%s: target %q must not include a path", where, s.Target)
		}
		if u.Host == c.Server.Listen {
			return nil, fmt.Errorf("%s: target points at our own listener, which would loop", where)
		}
		// A literal address can be checked here; a host name is checked after
		// DNS resolution in proxy/dial.go, which also covers rebinding.
		if ip := net.ParseIP(u.Hostname()); ip != nil && !ip.IsPrivate() && !ip.IsLoopback() {
			return nil, fmt.Errorf("%s: target %q is not a private address; refusing to run an open relay", where, s.Target)
		}

		u.Path, u.RawQuery, u.Fragment = "", "", ""
		targets[s.Subdomain] = u
	}
	return targets, nil
}
