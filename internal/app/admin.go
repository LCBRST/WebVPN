package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/LCBRST/webvpn/internal/auth"
	"github.com/LCBRST/webvpn/internal/config"
	"github.com/LCBRST/webvpn/internal/netutil"
	"github.com/LCBRST/webvpn/internal/portal"
	"github.com/LCBRST/webvpn/internal/store"
)

// adminCookieName is the admin page's own session cookie.
//
// It has to be a second cookie rather than the portal's: the portal one names
// the base domain, and a browser will never send it to a bare LAN address. The
// two also travel over different transports -- one over the tunnel, this one
// over plain HTTP inside the house -- and keeping them distinct makes that
// visible.
const adminCookieName = "webvpn_lan"

// minPasswordLen is enforced here and mirrored by minlength on the form, so the
// common case is caught in the browser and the rule is still real on the
// server.
const minPasswordLen = 8

// lanAuth is the admin page's session machinery.
//
// Deliberately not an auth.Handlers. That type bundles the login form, the
// post-login redirect and the cookie attributes together around a single
// CookieDomain field, which it uses both as the cookie's Domain attribute and
// as the list of hosts a post-login redirect may return to. Here those two
// wants are opposites: the cookie must stay host-only, and there is no redirect
// to validate because login always lands on /admin. Reusing it would mean
// pulling that field apart on the path that faces the internet.
type lanAuth struct {
	signer  *auth.Signer
	limiter *auth.Limiter
}

func newLANAuth(cfg *config.Config) *lanAuth {
	// The signing key is derived rather than shared, so the audience is
	// separated by construction: a token minted for the portal does not verify
	// here, and one minted here does not verify on the portal. Nothing in
	// session.go needs to know there are two of them.
	derived := hmac.New(sha256.New, cfg.SessionKey())
	derived.Write([]byte("webvpn-lan-admin"))

	return &lanAuth{
		signer: auth.NewSigner(derived.Sum(nil),
			cfg.Session.TTL.Std(), cfg.Session.RefreshAfter.Std(), cfg.Session.Epoch),
		// A separate limiter from the portal's, keyed on the address the kernel
		// gives us rather than on a forwarding header.
		limiter: auth.NewLimiter(limiterPolicy(cfg)),
	}
}

// adminView is what one render of the admin page needs: who is looking at it,
// anything to say at the top, and which site the form is pre-filled with.
type adminView struct {
	user    string
	errMsg  string
	notice  string
	editing *portal.AdminService
}

func (a *App) handleAdminLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.lanUser(w, r); ok {
		http.Redirect(w, r, "/admin", http.StatusFound)
		return
	}
	a.views.RenderAdminLogin(w, http.StatusOK, portal.AdminLoginData{})
}

func (a *App) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if !a.sameOrigin(r) {
		http.Error(w, "bad origin", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	username := store.TrimName(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	ip := lanIP(r)
	key := netutil.RateKey(ip)

	if v := a.lan.limiter.Allow(key); !v.OK {
		if s := v.RetrySeconds(); s > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(s))
		}
		a.logger.Warn("admin login throttled", "ip", ip, "locked", v.Locked, "retry_after", v.RetryAfter.String())
		a.views.RenderAdminLogin(w, http.StatusTooManyRequests, portal.AdminLoginData{
			Error: v.Message(),
		})
		return
	}

	// Serialise the bcrypt comparison, same as the portal login: it is a
	// quarter second of CPU and this is the step that costs it.
	release, ok := a.lan.limiter.Acquire(r.Context())
	if !ok {
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return
	}
	matched := a.store.Authenticate(username, password)
	release()

	// The admin check belongs here rather than after the redirect. Handing a
	// non-admin a session and then refusing every page they open is a worse
	// answer than refusing the login, and it would leave a cookie behind that
	// looks like access.
	if !matched || !a.store.IsAdmin(username) {
		a.lan.limiter.Fail(key)
		a.logger.Warn("admin login refused", "ip", ip, "user", username, "password_ok", matched)
		a.views.RenderAdminLogin(w, http.StatusUnauthorized, portal.AdminLoginData{
			Error: "用户名或密码不正确，或该账号不是管理员。",
		})
		return
	}
	a.lan.limiter.Succeed(key)

	token, err := a.lan.signer.Issue(username)
	if err != nil {
		a.logger.Error("issue admin session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setLANCookie(w, token)
	a.logger.Info("admin login ok", "ip", ip, "user", username)

	// A fixed destination, never a query parameter. There is no open redirect
	// to get wrong because there is no redirect to compute.
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *App) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if user, ok := a.lanUser(w, r); ok {
		a.logger.Info("admin logout", "ip", lanIP(r), "user", user)
	}
	a.clearLANCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}

	view := adminView{user: user}
	// ?edit=<subdomain> pre-fills the form instead of adding a second copy of
	// the same site. A name that is not there is ignored rather than an error:
	// it is a stale bookmark as often as it is a typo.
	if sub := strings.TrimSpace(r.URL.Query().Get("edit")); sub != "" {
		snap := a.store.Snapshot()
		if i := snap.IndexOfService(sub); i >= 0 {
			view.editing = adminService(snap.Services[i])
		}
	}
	a.renderAdmin(w, http.StatusOK, view)
}

func (a *App) handleAdminService(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if !a.sameOrigin(r) {
		a.renderAdmin(w, http.StatusForbidden, adminView{user: user, errMsg: "请求来源不对，已拒绝。"})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sub := strings.TrimSpace(r.PostFormValue("subdomain"))
	snap := a.store.Snapshot()

	if r.PostFormValue("action") == "delete" {
		i := snap.IndexOfService(sub)
		if i < 0 {
			a.renderAdmin(w, http.StatusNotFound, adminView{user: user, errMsg: "找不到站点 " + sub})
			return
		}
		name := snap.Services[i].Name
		snap.Services = append(snap.Services[:i], snap.Services[i+1:]...)
		// Drop the grant from every user as well. Left behind it would be inert
		// (the store only honours grants for services that exist) but it would
		// come back to life if the same subdomain were added again later, which
		// is a re-grant nobody asked for.
		revokeSubdomain(snap.Users, sub)

		if err := a.store.Replace(snap); err != nil {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
			return
		}
		a.rebuild()
		a.logger.Info("service deleted", "admin", user, "service", sub, "name", name)
		a.renderAdmin(w, http.StatusOK, adminView{user: user, notice: "已删除站点「" + name + "」"})
		return
	}

	// Save: either a new site, or the one named by `original`.
	edited := config.Service{
		Name:        strings.TrimSpace(r.PostFormValue("name")),
		Subdomain:   sub,
		Target:      strings.TrimSpace(r.PostFormValue("target")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Note:        r.PostFormValue("note"),
	}
	original := strings.TrimSpace(r.PostFormValue("original"))

	i := snap.IndexOfService(original)
	if i < 0 {
		snap.Services = append(snap.Services, edited)
	} else {
		renamedFrom := snap.Services[i].Subdomain
		snap.Services[i] = mergeService(snap.Services[i], edited)
		if renamedFrom != edited.Subdomain {
			// Carry the grants across. A rename that silently revoked everyone
			// would look like the site had been deleted.
			renameSubdomain(snap.Users, renamedFrom, edited.Subdomain)
		}
	}

	if err := a.store.Replace(snap); err != nil {
		// The validation message is the same one -check would print, so it is
		// shown as-is rather than paraphrased.
		a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
		return
	}
	a.rebuild()
	a.logger.Info("service saved", "admin", user, "service", edited.Subdomain, "target", edited.Target)
	a.renderAdmin(w, http.StatusOK, adminView{user: user, notice: "已保存站点「" + edited.Name + "」"})
}

func (a *App) handleAdminUser(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if !a.sameOrigin(r) {
		a.renderAdmin(w, http.StatusForbidden, adminView{user: user, errMsg: "请求来源不对，已拒绝。"})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := store.TrimName(r.PostFormValue("name"))
	snap := a.store.Snapshot()
	i := snap.IndexOfUser(name)
	action := r.PostFormValue("action")

	// A rename is deliberately not an action. The name is the identity the
	// session token carries and the key the access list is stored under, so
	// "rename" is really delete-and-add, and pretending otherwise is how one of
	// those two gets left behind.
	switch action {
	case "add":
		if i >= 0 {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: "用户 " + name + " 已存在。"})
			return
		}
		hash, err := a.hashPassword(r)
		if err != nil {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
			return
		}
		snap.Users = append(snap.Users, config.User{
			Name:         name,
			PasswordHash: hash,
			Admin:        r.PostFormValue("admin") == "true",
			// No services. New accounts start with access to nothing, and the
			// page says so where the account is created.
		})

	case "password":
		if i < 0 {
			a.renderAdmin(w, http.StatusNotFound, adminView{user: user, errMsg: "找不到用户 " + name})
			return
		}
		hash, err := a.hashPassword(r)
		if err != nil {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
			return
		}
		snap.Users[i].PasswordHash = hash

	case "admin":
		if i < 0 {
			a.renderAdmin(w, http.StatusNotFound, adminView{user: user, errMsg: "找不到用户 " + name})
			return
		}
		want := r.PostFormValue("admin") == "true"
		// The button is disabled on your own row; this is the server-side half
		// of that. The store separately refuses a state with no admin at all,
		// so this is the friendlier message, not the only guard.
		if !want && name == user {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: "不能取消自己的管理员身份。"})
			return
		}
		snap.Users[i].Admin = want

	case "delete":
		if i < 0 {
			a.renderAdmin(w, http.StatusNotFound, adminView{user: user, errMsg: "找不到用户 " + name})
			return
		}
		if name == user {
			a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: "不能删除自己。"})
			return
		}
		snap.Users = append(snap.Users[:i], snap.Users[i+1:]...)

	default:
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := a.store.Replace(snap); err != nil {
		a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
		return
	}
	a.logger.Info("user changed", "admin", user, "action", action, "user", name)
	a.renderAdmin(w, http.StatusOK, adminView{user: user, notice: noticeForUser(action, name)})
}

func (a *App) handleAdminAccess(w http.ResponseWriter, r *http.Request) {
	user, ok := a.requireAdmin(w, r)
	if !ok {
		return
	}
	if !a.sameOrigin(r) {
		a.renderAdmin(w, http.StatusForbidden, adminView{user: user, errMsg: "请求来源不对，已拒绝。"})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	name := store.TrimName(r.PostFormValue("name"))
	snap := a.store.Snapshot()
	i := snap.IndexOfUser(name)
	if i < 0 {
		a.renderAdmin(w, http.StatusNotFound, adminView{user: user, errMsg: "找不到用户 " + name})
		return
	}

	checked := make(map[string]bool, len(r.PostForm["grant"]))
	for _, sub := range r.PostForm["grant"] {
		checked[sub] = true
	}

	// Rebuilt from the service list rather than from the submitted values, so
	// the stored list can only ever name services that exist and is in the
	// order the page showed them. A stale entry cannot survive a save.
	grants := make([]string, 0, len(snap.Services))
	for _, svc := range snap.Services {
		if checked[svc.Subdomain] {
			grants = append(grants, svc.Subdomain)
		}
	}
	snap.Users[i].Services = grants

	if err := a.store.Replace(snap); err != nil {
		a.renderAdmin(w, http.StatusBadRequest, adminView{user: user, errMsg: err.Error()})
		return
	}
	a.logger.Info("access changed", "admin", user, "user", name, "services", grants)
	a.renderAdmin(w, http.StatusOK, adminView{
		user:   user,
		notice: "已更新 " + name + " 可访问的站点（" + strconv.Itoa(len(grants)) + " 个）。",
	})
}

// requireAdmin resolves the logged-in administrator or refuses the request.
//
// A cookie that verifies but belongs to an account that is no longer an admin
// gets a 403 rather than a redirect: the session is real, the permission is
// not, and bouncing them to a login form they would pass would just loop.
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	user, ok := a.lanUser(w, r)
	if !ok {
		http.Redirect(w, r, "/admin/login", http.StatusFound)
		return "", false
	}
	if !a.store.IsAdmin(user) {
		a.logger.Warn("admin page refused", "user", user, "ip", lanIP(r), "path", r.URL.Path)
		http.Error(w, "403 forbidden", http.StatusForbidden)
		return "", false
	}
	return user, true
}

// lanUser verifies the admin cookie and slides its expiry, the same way the
// portal session does. It writes a refreshed cookie when one is due and nothing
// at all when there is no session.
func (a *App) lanUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return "", false
	}
	user, expiry, ok := a.lan.signer.Verify(c.Value)
	if !ok {
		return "", false
	}
	if a.lan.signer.NeedsRefresh(expiry) {
		if token, err := a.lan.signer.Issue(user); err == nil {
			a.setLANCookie(w, token)
		}
	}
	return user, true
}

func (a *App) setLANCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:  adminCookieName,
		Value: token,
		Path:  "/",
		// No Domain on purpose: this stays bound to whatever address the page
		// was reached on, and there is no sibling host it needs to cover.
		HttpOnly: true,
		// False, and that is the honest setting rather than an oversight: this
		// listener speaks plain HTTP, so the cookie is readable by anyone on
		// the LAN who can watch the traffic. That is the documented cost of
		// keeping the admin page off the internet.
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(a.lan.signer.TTL().Seconds()),
	})
}

func (a *App) clearLANCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// sameOrigin reports whether a state-changing request came from this page.
//
// A CSRF token is the textbook answer and would be tighter, but it needs a
// per-session secret threaded through every form; SameSite=Lax already stops
// the cross-site form post this defends against, and this is a second lock on
// the same door that does not depend on the browser honouring SameSite. Both
// modern browsers send Origin on a form POST, so refusing a request that has
// neither Origin nor Referer costs an unusual setup and closes the "just omit
// the header" bypass.
func (a *App) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Header.Get("Referer")
	}
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return netutil.NormalizeHost(u.Host) == netutil.NormalizeHost(r.Host)
}

// hashPassword reads the submitted password and hashes it.
//
// The bcrypt call goes through the login limiter's semaphore so that it cannot
// be run concurrently with itself: the admin page is authenticated, but a
// script holding a stolen cookie should not be able to turn the machine into a
// space heater.
func (a *App) hashPassword(r *http.Request) (string, error) {
	password := r.PostFormValue("password")
	if len(password) < minPasswordLen {
		return "", errors.New("密码至少 " + strconv.Itoa(minPasswordLen) + " 位。")
	}

	release, ok := a.lan.limiter.Acquire(r.Context())
	if !ok {
		return "", errors.New("请求已取消")
	}
	defer release()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), store.BcryptCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func (a *App) renderAdmin(w http.ResponseWriter, status int, view adminView) {
	snap := a.store.Snapshot()

	services := make([]portal.AdminService, 0, len(snap.Services))
	for _, svc := range snap.Services {
		services = append(services, *adminService(svc))
	}

	users := make([]portal.AdminUser, 0, len(snap.Users))
	for _, u := range snap.Users {
		// The checkbox reflects what is stored, not what is effective. An admin
		// reaches everything whether or not it is ticked, and showing that as
		// "all ticked" would mean saving this form silently granted all of them
		// the moment the flag came off.
		granted := make(map[string]bool, len(u.Services))
		for _, sub := range u.Services {
			granted[sub] = true
		}

		access := make([]portal.AccessRow, 0, len(snap.Services))
		for _, svc := range snap.Services {
			access = append(access, portal.AccessRow{
				Subdomain: svc.Subdomain,
				Label:     svc.Name,
				Granted:   granted[svc.Subdomain],
			})
		}

		users = append(users, portal.AdminUser{
			Name:   u.Name,
			Admin:  u.Admin,
			Self:   u.Name == view.user,
			Access: access,
		})
	}

	a.views.RenderAdmin(w, status, portal.AdminData{
		User:       view.user,
		Error:      view.errMsg,
		Notice:     view.notice,
		Services:   services,
		Users:      users,
		Editing:    view.editing,
		BaseDomain: a.cfg.Server.BaseDomain,
	})
}

func adminService(svc config.Service) *portal.AdminService {
	return &portal.AdminService{
		Name:        svc.Name,
		Subdomain:   svc.Subdomain,
		Target:      svc.Target,
		Description: svc.Description,
		Note:        svc.Note,
	}
}

// mergeService overlays the fields the form edits onto the existing record,
// keeping everything it does not show.
//
// Without this, saving a one-word description change would quietly reset
// insecure_tls, rewrite_origin and the rest to their zero values, because the
// form never carried them.
func mergeService(existing, edited config.Service) config.Service {
	existing.Name = edited.Name
	existing.Subdomain = edited.Subdomain
	existing.Target = edited.Target
	existing.Description = edited.Description
	existing.Note = edited.Note
	return existing
}

// Both of these build a fresh slice rather than filtering in place. The
// snapshot is deep enough that in-place would be safe, but these lists are two
// or three entries long and an allocated slice cannot be got wrong.

func revokeSubdomain(users []config.User, subdomain string) {
	for i := range users {
		kept := make([]string, 0, len(users[i].Services))
		for _, sub := range users[i].Services {
			if sub != subdomain {
				kept = append(kept, sub)
			}
		}
		users[i].Services = kept
	}
}

func renameSubdomain(users []config.User, from, to string) {
	for i := range users {
		if !slices.Contains(users[i].Services, from) {
			continue
		}
		renamed := slices.Clone(users[i].Services)
		for j, sub := range renamed {
			if sub == from {
				renamed[j] = to
			}
		}
		users[i].Services = renamed
	}
}

func noticeForUser(action, name string) string {
	switch action {
	case "add":
		return "已添加用户 " + name + "，默认不能访问任何站点，请在下面的卡片里勾选。"
	case "password":
		return "已更新 " + name + " 的密码。"
	case "admin":
		return "已修改 " + name + " 的管理员身份。"
	case "delete":
		return "已删除用户 " + name + "。"
	default:
		return "已保存。"
	}
}

// lanIP is the client address as the kernel saw it.
//
// Not netutil.ClientIP: on this listener every client on the LAN can set
// CF-Connecting-IP and X-Forwarded-For, so those headers would report whatever
// the caller felt like. Same reasoning as LANMiddleware.
func lanIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
