package api

import (
	"net/http"
	"net/mail"
	"strings"
	"unicode"
	"unicode/utf8"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

func (s *Server) handlePHPApps(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, agentproto.PHPApps)
}

// appStarted carries the admin password; it is shown only this once.
type appStarted struct {
	Task          store.Task `json:"task"`
	AdminUser     string     `json:"admin_user,omitempty"`
	AdminPassword string     `json:"admin_password,omitempty"`
	AdminURL      string     `json:"admin_url,omitempty"`
}

// handleInstallPHPApp installs Joomla, Drupal, Moodle or Laravel into an empty PHP site.
func (s *Server) handleInstallPHPApp(w http.ResponseWriter, r *http.Request) {
	var o tasks.PHPAppOptions
	if !readJSON(w, r, &o) {
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	app, found := agentproto.FindPHPApp(o.App)
	switch {
	case !found:
		writeError(w, http.StatusBadRequest, "unknown_app", "choose an application")
		return
	case site.Kind != "php" || site.PHPVersion == nil:
		writeError(w, http.StatusConflict, "app_needs_php", "this application needs a PHP site; switch the site to PHP first")
		return
	case !agentproto.PHPAtLeast(*site.PHPVersion, app.MinPHP):
		writeError(w, http.StatusConflict, "app_needs_newer_php", app.Name+" needs PHP "+app.MinPHP+" or newer")
		return
	case site.App != nil:
		writeError(w, http.StatusConflict, "app_installed", "an application is already installed on this site")
		return
	}
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
	if utf8.RuneCountInString(o.Title) > 100 || strings.IndexFunc(o.Title, unicode.IsControl) >= 0 {
		writeError(w, http.StatusBadRequest, "invalid_wp_title", "the title must be up to 100 characters on one line")
		return
	}
	if app.Admin {
		if o.AdminEmail == "" {
			o.AdminEmail = u.Email
		}
		if app.ID == "moodle" {
			o.AdminUser = strings.ToLower(o.AdminUser) // Moodle user names are lowercase
		}
		if !wpAdminUserRE.MatchString(o.AdminUser) {
			writeError(w, http.StatusBadRequest, "invalid_wp_admin_user", "use 3–60 letters, digits and . _ @ -")
			return
		}
		if a, err := mail.ParseAddress(o.AdminEmail); err != nil || a.Address != o.AdminEmail {
			writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
			return
		}
	} else {
		o.AdminUser, o.AdminEmail = "", ""
	}
	t, password, err := s.opts.Tasks.QueuePHPAppInstall(r.Context(), site, o, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.app_install", site.Domain, map[string]any{"site_id": site.ID, "task_id": t.ID, "app": app.ID})
	out := appStarted{Task: t}
	if app.Admin {
		out.AdminUser, out.AdminPassword, out.AdminURL = o.AdminUser, password, "https://"+site.Domain+app.AdminPath
	}
	writeJSON(w, http.StatusAccepted, out)
}
