package app

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// The tests for the part of the app that did not exist before this feature: who
// may reach which service, and the LAN-side admin page that decides it.

const (
	// lanHost is the address the admin fixture's second listener is configured
	// with, and therefore the Host the admin requests are made to.
	lanHost   = "192.168.1.5"
	otherHost = "other-vpn.example.com"
	bobUser   = "bob"
)

// adminApp is the fixture these tests run against: two services, one admin and
// one ordinary account.
//
// Note what is deliberately absent -- no per-user `services:` on bob. The seed
// hands every account every service on purpose, so a test that wants "bob
// cannot reach this" has to take it away through the admin page, which is what
// an operator would do and what the access handler is for.
func adminApp(t *testing.T, backendURL string) *App {
	t.Helper()

	adminHash, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash admin password: %v", err)
	}
	bobHash, err := bcrypt.GenerateFromPassword([]byte(testPass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash bob password: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

	doc := fmt.Sprintf(`
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
    admin: true
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
  - name: "Other App"
    subdomain: "other-vpn"
    target: %q
    description: "second fixture"
lan_index:
  listen: "%s:80"
  host: %q
`, baseDomain, portalHost, key,
		testUser, string(adminHash),
		bobUser, string(bobHash),
		backendURL, backendURL,
		lanHost, lanHost)

	return newAppFromDoc(t, doc)
}

func adminGet(a *App, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "http://"+lanHost+path, nil)
	r.Host = lanHost
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.LANHandler().ServeHTTP(w, r)
	return w
}

// adminPost submits a form the way the page does. Every handler that changes
// something checks the Origin, so this sets it as a browser would; the test that
// is about that check builds its own request instead.
func adminPost(a *App, path string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://"+lanHost+path, strings.NewReader(form.Encode()))
	r.Host = lanHost
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://"+lanHost)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.LANHandler().ServeHTTP(w, r)
	return w
}

func adminCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatalf("no admin cookie in response; headers were %v", w.Result().Header)
	return nil
}

func adminSession(t *testing.T, a *App, user, pass string) *http.Cookie {
	t.Helper()
	w := adminPost(a, "/admin/login", url.Values{"username": {user}, "password": {pass}}, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("admin login as %s: status = %d, want 303; body %s", user, w.Code, w.Body)
	}
	return adminCookie(t, w)
}

// grantAccess replaces a user's service list through the admin page. Passing no
// subdomains revokes everything, which is how these tests set up "no access" --
// the seed starts everyone with all of it.
func grantAccess(t *testing.T, a *App, cookie *http.Cookie, user string, subdomains ...string) {
	t.Helper()
	form := url.Values{"name": {user}}
	for _, sub := range subdomains {
		form.Add("grant", sub)
	}
	if w := adminPost(a, "/admin/access", form, cookie); w.Code != http.StatusOK {
		t.Fatalf("grant %v to %s: status = %d; body %s", subdomains, user, w.Code, w.Body)
	}
}

func TestServiceRequiresAccessNotJustALogin(t *testing.T) {
	// The change this whole feature is about: being logged in used to be the
	// same as being allowed everywhere.
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	grantAccess(t, a, admin, bobUser)

	c := sessionCookie(t, postLogin(a, bobUser, testPass))
	if got := get(a, appHost, "/", c).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a logged-in user with no grants", got)
	}
}

func TestAccessChangesLandOnTheNextRequest(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	grantAccess(t, a, admin, bobUser)

	// One cookie for every request below. The point is that the token never
	// changes, so nothing here can be explained by a re-issue.
	c := sessionCookie(t, postLogin(a, bobUser, testPass))
	if got := get(a, appHost, "/", c).Code; got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 before the grant", got)
	}

	grantAccess(t, a, admin, bobUser, "app-vpn")
	if got := get(a, appHost, "/", c).Code; got != http.StatusOK {
		t.Errorf("status = %d, want 200 on the same cookie after a grant", got)
	}
	// Access to one service is not access to the others.
	if got := get(a, otherHost, "/", c).Code; got != http.StatusForbidden {
		t.Errorf("other service: status = %d, want 403", got)
	}

	grantAccess(t, a, admin, bobUser)
	if got := get(a, appHost, "/", c).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 on the same cookie after a revocation", got)
	}
}

func TestAdminReachesEveryServiceWithoutAGrant(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	// Strip the admin's own grant list too, so the 200s below can only come
	// from the admin flag and not from a leftover grant.
	grantAccess(t, a, admin, testUser)

	c := sessionCookie(t, postLogin(a, testUser, testPass))
	for _, host := range []string{appHost, otherHost} {
		if got := get(a, host, "/", c).Code; got != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 for an admin with no grants", host, got)
		}
	}
}

func TestDeletedUserLosesTheirLiveSession(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	// The token is stateless and nothing revokes it on delete, so it still
	// verifies afterwards. What refuses the request is the authorization check,
	// and that is the thing worth pinning down here.
	c := sessionCookie(t, postLogin(a, bobUser, testPass))
	if got := get(a, appHost, "/", c).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200 before the delete", got)
	}

	w := adminPost(a, "/admin/users", url.Values{"action": {"delete"}, "name": {bobUser}}, admin)
	if w.Code != http.StatusOK {
		t.Fatalf("delete bob: status = %d; body %s", w.Code, w.Body)
	}

	if got := get(a, appHost, "/", c).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a deleted user's live token", got)
	}
}

func TestPortalListsOnlyWhatTheUserMayReach(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)
	grantAccess(t, a, admin, bobUser, "app-vpn")

	c := sessionCookie(t, postLogin(a, bobUser, testPass))
	body := get(a, portalHost, "/", c).Body.String()

	if !strings.Contains(body, "Test App") {
		t.Errorf("the portal omits a service the user may reach; body %s", body)
	}
	if strings.Contains(body, "Other App") {
		t.Errorf("the portal offers a card that would only 403; body %s", body)
	}
}

func TestAdminCannotLockThemselvesOut(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	// No `admin` value on the first form means want=false, which is the demote
	// the page disables the button for.
	tests := []struct {
		what string
		form url.Values
	}{
		{"demote self", url.Values{"action": {"admin"}, "name": {testUser}}},
		{"delete self", url.Values{"action": {"delete"}, "name": {testUser}}},
	}
	for _, tt := range tests {
		if got := adminPost(a, "/admin/users", tt.form, admin).Code; got != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tt.what, got)
		}
	}

	if got := adminGet(a, "/admin", admin).Code; got != http.StatusOK {
		t.Errorf("status = %d, want 200: the admin lost their own access", got)
	}
}

func TestAdminPageRequiresAnAdminSession(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)

	w := adminGet(a, "/admin", nil)
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 to the login page", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", loc)
	}

	// A correct password on an account that is not an admin is refused at the
	// form, rather than handing out a session that opens nothing.
	w = adminPost(a, "/admin/login", url.Values{"username": {bobUser}, "password": {testPass}}, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("non-admin login: status = %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == adminCookieName && c.Value != "" {
			t.Error("a refused login still set a session cookie")
		}
	}
}

func TestDemotedAdminLosesTheirLiveSession(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	promote := url.Values{"action": {"admin"}, "name": {bobUser}, "admin": {"true"}}
	if w := adminPost(a, "/admin/users", promote, admin); w.Code != http.StatusOK {
		t.Fatalf("promote bob: status = %d; body %s", w.Code, w.Body)
	}
	bob := adminSession(t, a, bobUser, testPass)
	if got := adminGet(a, "/admin", bob).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200 while bob is an admin", got)
	}

	if w := adminPost(a, "/admin/users", url.Values{"action": {"admin"}, "name": {bobUser}}, admin); w.Code != http.StatusOK {
		t.Fatalf("demote bob: status = %d; body %s", w.Code, w.Body)
	}

	// The cookie is a valid session either way, so this has to be a 403 and not
	// a redirect to a login form bob would only pass again.
	if got := adminGet(a, "/admin", bob).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a demoted admin's live session", got)
	}
}

func TestAdminRejectsRequestsFromElsewhere(t *testing.T) {
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)
	admin := adminSession(t, a, testUser, testPass)

	// A revoke-everything request, with the Origin under the test's control.
	send := func(origin string) int {
		r := httptest.NewRequest(http.MethodPost, "http://"+lanHost+"/admin/access",
			strings.NewReader(url.Values{"name": {bobUser}}.Encode()))
		r.Host = lanHost
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		r.AddCookie(admin)
		w := httptest.NewRecorder()
		a.LANHandler().ServeHTTP(w, r)
		return w.Code
	}

	grantAccess(t, a, admin, bobUser, "app-vpn")
	bob := sessionCookie(t, postLogin(a, bobUser, testPass))

	for _, origin := range []string{"http://evil.example", ""} {
		if got := send(origin); got != http.StatusForbidden {
			t.Errorf("Origin %q: status = %d, want 403", origin, got)
		}
		// Refused means refused: the access list has to be exactly as it was.
		if got := get(a, appHost, "/", bob).Code; got != http.StatusOK {
			t.Errorf("Origin %q: a rejected post changed the access list", origin)
		}
	}

	if got := send("http://" + lanHost); got != http.StatusOK {
		t.Errorf("same Origin: status = %d, want 200", got)
	}
	if got := get(a, appHost, "/", bob).Code; got != http.StatusForbidden {
		t.Errorf("status = %d, want 403 after an accepted revoke", got)
	}
}

func TestAdminAndPortalSessionsAreNotInterchangeable(t *testing.T) {
	// The signing keys are derived rather than shared, so this is a property of
	// how they are built rather than of a check somewhere.
	srv, _ := newBackend(t)
	a := adminApp(t, srv.URL)

	portal := sessionCookie(t, postLogin(a, testUser, testPass))
	admin := adminSession(t, a, testUser, testPass)

	asAdminCookie := &http.Cookie{Name: adminCookieName, Value: portal.Value}
	if got := adminGet(a, "/admin", asAdminCookie).Code; got != http.StatusFound {
		t.Errorf("status = %d, want 302: a portal token was accepted on the admin page", got)
	}

	asPortalCookie := &http.Cookie{Name: "webvpn_session", Value: admin.Value}
	if got := get(a, portalHost, "/", asPortalCookie).Code; got != http.StatusFound {
		t.Errorf("status = %d, want 302: an admin token was accepted as a portal session", got)
	}
}
