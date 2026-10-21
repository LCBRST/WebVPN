// Package portal renders the two pages a browser sees from this proxy itself
// rather than from a backend: the login form and the index of services.
package portal

import (
	"bytes"
	"embed"
	"html/template"
	"net/http"
)

//go:embed templates/*.html
var files embed.FS

// Views holds the parsed templates. Every page is parsed as its own template
// set because each is a complete document with its own <head>.
type Views struct {
	login      *template.Template
	portal     *template.Template
	admin      *template.Template
	adminLogin *template.Template
}

func NewViews() (*Views, error) {
	parse := func(name string) (*template.Template, error) {
		return template.ParseFS(files, "templates/"+name)
	}

	login, err := parse("login.html")
	if err != nil {
		return nil, err
	}
	index, err := parse("portal.html")
	if err != nil {
		return nil, err
	}
	admin, err := parse("admin.html")
	if err != nil {
		return nil, err
	}
	adminLogin, err := parse("adminlogin.html")
	if err != nil {
		return nil, err
	}
	return &Views{login: login, portal: index, admin: admin, adminLogin: adminLogin}, nil
}

// LoginData is what the login page needs.
type LoginData struct {
	// Error is shown above the form. Empty on a first visit.
	Error string
	// Next is where to go after a successful login, passed through the form as
	// a hidden field. Always validated by the caller before it gets here.
	Next string
}

// PortalData is what the service index needs.
type PortalData struct {
	// User is empty on the LAN index, which has no login. The template uses it
	// to decide whether to show the log-out link.
	User     string
	Services []Service
	// LAN marks the page as the unauthenticated LAN copy of the index, so the
	// header does not offer a log-out that would go nowhere.
	LAN bool
}

// Service is one entry in the index.
type Service struct {
	Name        string
	Description string
	URL         string
	// Note surfaces the per-service caveats from the config, which is the
	// place they are easiest to keep.
	Note string
}

// AdminData is what the admin page needs. Everything it shows comes from one
// consistent read of the state, so the sections cannot disagree with each other.
type AdminData struct {
	// User is the logged-in administrator.
	User string
	// Error and Notice are mutually exclusive in practice: one is a rejected
	// save, the other a save that went through.
	Error  string
	Notice string

	Services []AdminService
	Users    []AdminUser
	// Editing is the service the form is currently pre-filled with, or nil when
	// the form is in "add" mode.
	Editing *AdminService
	// BaseDomain is shown in the form's hint, so it is obvious what the public
	// host name will come out as before the site is added.
	BaseDomain string
}

// AdminService is a service as the admin page shows it. Deliberately not
// config.Service: it carries only the fields the form can edit. The rest of a
// service's settings are preserved untouched when it is saved, rather than
// being round-tripped through form fields nobody is looking at.
type AdminService struct {
	Name        string
	Subdomain   string
	Target      string
	Description string
	Note        string
}

// AdminUser is one row of the user list.
type AdminUser struct {
	Name  string
	Admin bool
	// Self marks the logged-in account, so the page can disable the controls
	// that would lock its owner out.
	Self bool
	// Access is pre-expanded against the current service list, so the template
	// does not have to cross-reference two ranges to draw the checkboxes.
	Access []AccessRow
}

// AccessRow is one service checkbox on one user's row.
type AccessRow struct {
	Subdomain string
	Label     string
	Granted   bool
}

// AdminLoginData is the admin login page.
type AdminLoginData struct {
	Error string
	// Notice carries the one thing worth saying before a first login, such as
	// there being no administrator yet.
	Notice string
}

func (v *Views) RenderLogin(w http.ResponseWriter, status int, data LoginData) {
	render(w, v.login, status, data)
}

func (v *Views) RenderPortal(w http.ResponseWriter, status int, data PortalData) {
	render(w, v.portal, status, data)
}

func (v *Views) RenderAdmin(w http.ResponseWriter, status int, data AdminData) {
	render(w, v.admin, status, data)
}

func (v *Views) RenderAdminLogin(w http.ResponseWriter, status int, data AdminLoginData) {
	render(w, v.adminLogin, status, data)
}

// render executes into a buffer first. Writing straight to the ResponseWriter
// would commit a status code before the template had a chance to fail, so a
// broken template would produce half a page with a 200 on it.
func render(w http.ResponseWriter, t *template.Template, status int, data any) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// These pages are per-session; Cloudflare sits in front and must not keep
	// a copy.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")

	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
