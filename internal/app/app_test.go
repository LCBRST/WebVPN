package app

import (
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/LCBRST/webvpn/internal/config"
)

const (
	baseDomain = "example.com"
	portalHost = "vpn.example.com"
	appHost    = "app-vpn.example.com"
	testUser   = "admin"
	testPass   = "correct horse battery staple"
)

// backendState records what the backend saw, so the tests can assert on what
// was actually forwarded rather than only on what came back.
type backendState struct {
	sawSessionCookie bool
}

func newBackend(t *testing.T) (*httptest.Server, *backendState) {
	t.Helper()
	state := &backendState{}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("webvpn_session"); err == nil {
			state.sawSessionCookie = true
		}
		fmt.Fprint(w, "backend body")
	})
	mux.HandleFunc("/setcookie", func(w http.ResponseWriter, r *http.Request) {
		// An appliance stamping its own address into Domain, which a browser
		// would reject outright.
		http.SetCookie(w, &http.Cookie{
			Name: "device", Value: "v", Path: "/",
			Domain: "127.0.0.1", HttpOnly: true,
		})
		fmt.Fprint(w, "set")
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		// An absolute redirect naming its own address.
		http.Redirect(w, r, "http://"+r.Host+"/landing", http.StatusFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, state
}

func newTestApp(t *testing.T, backendURL string) *App {
	t.Helper()
	return newTestAppWith(t, backendURL, "")
}

// newTestAppWith appends extraYAML to the fixture config, for the tests that
// need a section the default fixture does not carry.
func newTestAppWith(t *testing.T, backendURL, extraYAML string) *App {
	t.Helper()
	return newAppFromDoc(t, testDoc(t, backendURL, extraYAML))
}

// testDoc is the shared fixture as a config document, so a test can rewrite one
// field before loading it.
func testDoc(t *testing.T, backendURL, extraYAML string) string {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	return fmt.Sprintf(`
server:
  listen: "127.0.0.1:0"
  base_domain: %q
  portal_host: %q
  public_scheme: "http"
  allow_loopback_targets: true
session:
  cookie_name: "webvpn_session"
  signing_key: %q
users:
  - name: %q
    password_hash: %q
login:
  attempts_per_minute: 100
  max_concurrent: 1
services:
  - name: "Test App"
    subdomain: "app-vpn"
    target: %q
    description: "fixture"
`, baseDomain, portalHost, key, testUser, string(hash), backendURL) + extraYAML
}

// httpsApp is the fixture with public_scheme https, which arms the transport
// guards.
func httpsApp(t *testing.T, backendURL, extraYAML string) *App {
	t.Helper()
	doc := strings.Replace(testDoc(t, backendURL, extraYAML),
		`public_scheme: "http"`, `public_scheme: "https"`, 1)
	return newAppFromDoc(t, doc)
}

// newAppFromDoc loads a config document and builds the app from it.
//
// The admin tests need two services and two accounts, and one YAML document
// cannot carry `users:` twice, so they write a whole document rather than
// appending a section to the shared fixture.
func newAppFromDoc(t *testing.T, doc string) *App {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// The state file defaults to the config's own directory, which is this
	// test's temp dir, so each app seeds into private ground and the cleanup is
	// automatic.
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("build app: %v", err)
	}
	return a
}

func get(a *App, host, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
	r.Host = host
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func postLogin(a *App, user, pass string) *httptest.ResponseRecorder {
	form := url.Values{"username": {user}, "password": {pass}, "next": {""}}
	r := httptest.NewRequest(http.MethodPost, "http://"+portalHost+"/login", strings.NewReader(form.Encode()))
	r.Host = portalHost
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func sessionCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == "webvpn_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no session cookie in response; headers were %v", w.Result().Header)
	return nil
}

func TestUnknownHostIs404(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	if got := get(a, "nope.example.com", "/", nil).Code; got != http.StatusNotFound {
		t.Errorf("status = %d, want 404", got)
	}
	if got := get(a, "evil.example.org", "/", nil).Code; got != http.StatusNotFound {
		t.Errorf("off-domain host: status = %d, want 404", got)
	}
}

func TestServiceRequiresLogin(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	w := get(a, appHost, "/", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to the login page", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://"+portalHost+"/login") {
		t.Errorf("Location = %q, want the portal login page", loc)
	}
}

func TestXHRGetsUnauthorizedNotRedirect(t *testing.T) {
	// A fetch() that receives a 302 follows it silently and hands the login
	// page's HTML back to the caller as a 200, which is very hard to debug.
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	r := httptest.NewRequest(http.MethodGet, "http://"+appHost+"/api", nil)
	r.Host = appHost
	r.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestLoginRejectsWrongPassword(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	for _, password := range []string{"wrong", ""} {
		if got := postLogin(a, testUser, password).Code; got != http.StatusUnauthorized {
			t.Errorf("password %q: status = %d, want 401", password, got)
		}
	}
}

func TestLoginIssuesCookieForBaseDomain(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	w := postLogin(a, testUser, testPass)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %s", w.Code, w.Body)
	}

	c := sessionCookie(t, w)
	// The base domain, not the portal host: this is what makes one login cover
	// every service subdomain.
	if c.Domain != baseDomain {
		t.Errorf("cookie Domain = %q, want %q", c.Domain, baseDomain)
	}
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v, want Lax", c.SameSite)
	}
}

func TestSessionWorksAcrossSubdomains(t *testing.T) {
	// The whole point of naming the base domain in the cookie.
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	c := sessionCookie(t, postLogin(a, testUser, testPass))

	w := get(a, appHost, "/", c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
	}
	if got := w.Body.String(); !strings.Contains(got, "backend body") {
		t.Errorf("body = %q, want the backend's response", got)
	}
}

func TestSessionCookieIsNotForwardedToBackend(t *testing.T) {
	srv, state := newBackend(t)
	a := newTestApp(t, srv.URL)

	c := sessionCookie(t, postLogin(a, testUser, testPass))
	get(a, appHost, "/", c)

	if state.sawSessionCookie {
		t.Error("the session cookie was forwarded to the backend")
	}
}

func TestBackendCookieDomainIsStripped(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	c := sessionCookie(t, postLogin(a, testUser, testPass))
	w := get(a, appHost, "/setcookie", c)

	var device *http.Cookie
	for _, got := range w.Result().Cookies() {
		if got.Name == "device" {
			device = got
		}
	}
	if device == nil {
		t.Fatal("the backend's cookie did not survive the proxy")
	}
	// Left as a host-only cookie, so the browser binds it to the public
	// subdomain instead of discarding it.
	if device.Domain != "" {
		t.Errorf("cookie Domain = %q, want it dropped", device.Domain)
	}
}

func TestBackendRedirectIsMadeRelative(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	c := sessionCookie(t, postLogin(a, testUser, testPass))
	w := get(a, appHost, "/redirect", c)

	if got := w.Header().Get("Location"); got != "/landing" {
		t.Errorf("Location = %q, want /landing", got)
	}
}

func TestPortalListsServices(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	c := sessionCookie(t, postLogin(a, testUser, testPass))
	w := get(a, portalHost, "/", c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Test App") {
		t.Errorf("portal does not list the service; body %s", body)
	}
	if !strings.Contains(body, "http://"+appHost+"/") {
		t.Errorf("portal link is wrong; body %s", body)
	}
}

func TestLoginThrottlesRepeatedFailures(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL)

	// The default failure threshold is 5 within the failure window, so this
	// exercises the lockout through the real HTTP path.
	var throttled bool
	for i := 0; i < 20; i++ {
		if postLogin(a, testUser, "wrong").Code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Error("repeated failed logins were never throttled")
	}
}

// TestLoginLockoutSurvivesACorrectPassword is the regression test for the
// reported bypass: refreshing the page (a fresh GET plus a new POST) must not
// make the server forget that this address just burned its failure budget.
func TestLoginLockoutSurvivesACorrectPassword(t *testing.T) {
	srv, _ := newBackend(t)
	a := newTestApp(t, srv.URL) // default max_failures = 5

	for i := 0; i < 5; i++ {
		if w := postLogin(a, testUser, "wrong"); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i+1, w.Code)
		}
	}

	w := postLogin(a, testUser, testPass)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password during lockout: status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 response has no Retry-After header")
	}
	if !strings.Contains(w.Body.String(), "锁定") {
		t.Error("lockout response does not say the account is temporarily locked")
	}
}

func TestPlainHTTPIsRedirectedToHTTPS(t *testing.T) {
	srv, _ := newBackend(t)
	a := httpsApp(t, srv.URL, "")

	tests := []struct {
		host, method, target string
	}{
		{portalHost, http.MethodGet, "/login?next=%2F"},
		{portalHost, http.MethodPost, "/login"},
		{appHost, http.MethodGet, "/some/path?q=1"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(tt.method, "http://"+tt.host+tt.target,
			strings.NewReader("username=admin&password=x"))
		r.Host = tt.host
		r.Header.Set("CF-Visitor", `{"scheme":"http"}`)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)

		if w.Code != http.StatusPermanentRedirect {
			t.Errorf("%s %s: status = %d, want 308", tt.method, tt.target, w.Code)
			continue
		}
		want := "https://" + tt.host + tt.target
		if got := w.Header().Get("Location"); got != want {
			t.Errorf("%s %s: Location = %q, want %q", tt.method, tt.target, got, want)
		}
	}
}

// A WebSocket handshake cannot follow a redirect: the client fails the
// connection instead of replaying the upgrade at the new URL. A page that is
// still served over plain http would lose its socket if the transport guard
// answered the upgrade itself, so upgrades must reach the proxy untouched.
func TestWebSocketUpgradeIsNotRedirected(t *testing.T) {
	srv, _ := newBackend(t)
	a := httpsApp(t, srv.URL, "")

	token, err := a.auth.Signer.Issue(testUser)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet,
		"http://"+appHost+"/socket.io/?EIO=4&transport=websocket", nil)
	r.Host = appHost
	r.Header.Set("X-Forwarded-Proto", "http")
	r.Header.Set("Connection", "Upgrade")
	r.Header.Set("Upgrade", "websocket")
	r.Header.Set("Sec-WebSocket-Version", "13")
	r.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	r.AddCookie(&http.Cookie{Name: a.cfg.Session.CookieName, Value: token})
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)

	if w.Code == http.StatusPermanentRedirect {
		t.Fatalf("upgrade redirected to %q, want it passed through", w.Header().Get("Location"))
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 from the backend", w.Code)
	}
}

func TestHTTPSRequestsGetHSTS(t *testing.T) {
	srv, _ := newBackend(t)
	a := httpsApp(t, srv.URL, "")

	// Cloudflare sets both headers; either one is enough to tell the app the
	// browser is on https even though the origin hop is plain http.
	for _, header := range []struct{ name, value string }{
		{"X-Forwarded-Proto", "https"},
		{"CF-Visitor", `{"scheme":"https"}`},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://"+portalHost+"/login", nil)
		r.Host = portalHost
		r.Header.Set(header.name, header.value)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", header.name, w.Code)
		}
		if got := w.Header().Get("Strict-Transport-Security"); got != hstsValue {
			t.Errorf("%s: HSTS header = %q, want %q", header.name, got, hstsValue)
		}
	}
}

// The admin listener is plain http by design on the LAN, so the public
// transport guards must not reach it.
func TestLANListenerIsNotRedirectedOrPinned(t *testing.T) {
	srv, _ := newBackend(t)
	a := httpsApp(t, srv.URL, `
lan_index:
  listen: "192.168.1.5:80"
  host: "192.168.1.5"
`)

	w := lanGet(a, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS header on the LAN listener: %q", got)
	}
}

// lanApp is the fixture with the second listener configured. The fixture
// backend is an httptest server on loopback, so this also exercises the
// loopback rewrite.
func lanApp(t *testing.T, backendURL string) *App {
	t.Helper()
	return newTestAppWith(t, backendURL, `
lan_index:
  listen: "192.168.1.5:80"
  host: "192.168.1.5"
`)
}

func lanGet(a *App, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://192.168.1.5"+path, nil)
	w := httptest.NewRecorder()
	a.LANHandler().ServeHTTP(w, r)
	return w
}

func TestLANIndexNeedsNoSession(t *testing.T) {
	srv, _ := newBackend(t)
	a := lanApp(t, srv.URL)

	w := lanGet(a, "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with no session at all", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Test App") {
		t.Errorf("the LAN index does not list the service; body %s", body)
	}
	// The card carries the backend's own address with the configured host
	// swapped in, and never the public subdomain.
	if !strings.Contains(body, "192.168.1.5") {
		t.Errorf("card link is not the LAN address; body %s", body)
	}
	if strings.Contains(body, appHost) {
		t.Errorf("card link is still the public subdomain; body %s", body)
	}
	if strings.Contains(body, "退出登录") {
		t.Error("the LAN index offers a log-out link it has no session for")
	}
}

// The security property of mounting a separate handler: the only paths that
// exist on this listener are the index and the admin page, so the portal's
// routes and the backends are not reachable here at all.
func TestLANIndexServesOnlyTheRootPath(t *testing.T) {
	srv, _ := newBackend(t)
	a := lanApp(t, srv.URL)

	for _, path := range []string{"/login", "/logout", "/api", "/admin/nope", "/anything"} {
		if got := lanGet(a, path).Code; got != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, got)
		}
	}
}

func TestLANIndexRejectsNonGET(t *testing.T) {
	srv, _ := newBackend(t)
	a := lanApp(t, srv.URL)

	r := httptest.NewRequest(http.MethodPost, "http://192.168.1.5/", nil)
	w := httptest.NewRecorder()
	a.LANHandler().ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestLANMiddlewareRefusesClientsFromOutsideTheLAN(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := LANMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "page")
	}))

	tests := []struct {
		remote string
		want   int
	}{
		{"192.168.1.20:5555", http.StatusOK},
		{"10.0.0.7:5555", http.StatusOK},
		{"172.16.5.4:5555", http.StatusOK},
		{"127.0.0.1:5555", http.StatusOK}, // the machine itself, over ssh
		{"8.8.8.8:5555", http.StatusForbidden},
		{"[2606:4700::1111]:443", http.StatusForbidden},
	}

	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "http://192.168.1.5/", nil)
		r.RemoteAddr = tt.remote
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != tt.want {
			t.Errorf("RemoteAddr %s: status = %d, want %d", tt.remote, w.Code, tt.want)
		}
	}
}
