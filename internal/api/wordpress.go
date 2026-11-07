package api

import (
	"net/http"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

var (
	wpAdminUserRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,60}$`)
	wpLocaleRE    = regexp.MustCompile(`^[a-z]{2,3}(_[A-Z]{2})?$`)
)

// wordPressStarted carries the admin password; it is shown only this once.
type wordPressStarted struct {
	Task          store.Task `json:"task"`
	AdminUser     string     `json:"admin_user"`
	AdminPassword string     `json:"admin_password"`
	AdminURL      string     `json:"admin_url"`
}

func (s *Server) handleInstallWordPress(w http.ResponseWriter, r *http.Request) {
	var o tasks.WordPressOptions
	if !readJSON(w, r, &o) {
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if site.Kind != "php" {
		writeError(w, http.StatusConflict, "wordpress_needs_php", "WordPress needs a PHP site; switch the site to PHP first")
		return
	}
	if site.App != nil {
		writeError(w, http.StatusConflict, "app_installed", "an application is already installed on this site")
		return
	}
	// WordPress brings its own database, which counts against the owner's package.
	pkg, err := s.packageFor(r, site.OwnerID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.allowNew(w, r, placement{OwnerID: site.OwnerID, Package: pkg}, "database", site.ServerID) {
		return
	}
	u := currentUser(r)
	o.SiteID = site.ID
	o.Title = strings.TrimSpace(o.Title)
	if o.Title == "" {
		o.Title = toDisplay(site.Domain)
	}
	if o.AdminEmail == "" {
		o.AdminEmail = u.Email
	}
	if o.Locale == "" {
		o.Locale = "en_US"
	}
	switch {
	case !wpAdminUserRE.MatchString(o.AdminUser):
		writeError(w, http.StatusBadRequest, "invalid_wp_admin_user", "use 3–60 letters, digits and . _ @ -")
		return
	case utf8.RuneCountInString(o.Title) > 100 || strings.IndexFunc(o.Title, unicode.IsControl) >= 0:
		writeError(w, http.StatusBadRequest, "invalid_wp_title", "the title must be up to 100 characters on one line")
		return
	case !wpLocaleRE.MatchString(o.Locale):
		writeError(w, http.StatusBadRequest, "invalid_wp_locale", "unknown language")
		return
	}
	if a, err := mail.ParseAddress(o.AdminEmail); err != nil || a.Address != o.AdminEmail {
		writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
		return
	}
	t, password, err := s.opts.Tasks.QueueWordPressInstall(r.Context(), site, o, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_install", site.Domain, map[string]any{"site_id": site.ID, "task_id": t.ID, "admin_user": o.AdminUser})
	writeJSON(w, http.StatusAccepted, wordPressStarted{
		Task: t, AdminUser: o.AdminUser, AdminPassword: password, AdminURL: "https://" + site.Domain + "/wp-admin/",
	})
}
