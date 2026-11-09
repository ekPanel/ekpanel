package api

import (
	"net/http"

	"ekpanel/internal/auth"
)

type ftpRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetFTP turns a site's FTP login on (with a new password, shown once) or off.
func (s *Server) handleSetFTP(w http.ResponseWriter, r *http.Request) {
	var req ftpRequest
	if !readJSON(w, r, &req) {
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if req.Enabled {
		pkg, err := s.packageFor(r, site.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if pkg != nil && !pkg.FTP {
			writeError(w, http.StatusForbidden, "ftp_not_allowed", "the package does not include FTP")
			return
		}
	}
	var password string
	var hash *string
	if req.Enabled && !site.FTPEnabled {
		password = auth.NewDatabasePassword()
		h, err := auth.LinuxPasswordHash(password)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		hash = &h
	}
	site, err := s.store.SetSiteFTP(r.Context(), site.ID, req.Enabled, hash)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	action := "site.ftp_disabled"
	if req.Enabled {
		action = "site.ftp_enabled"
	}
	s.audit(r, action, site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, siteWithPassword{Site: site, Password: password})
}

func (s *Server) handleResetFTPPassword(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if !site.FTPEnabled {
		writeError(w, http.StatusConflict, "ftp_disabled", "turn on FTP first")
		return
	}
	password := auth.NewDatabasePassword()
	h, err := auth.LinuxPasswordHash(password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	site, err = s.store.SetSiteFTP(r.Context(), site.ID, true, &h)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.ftp_password_reset", site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, siteWithPassword{Site: site, Password: password})
}
