package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/store"
)

// Moving a website (with chosen databases) to another server. Administrators only.

type moveRequest struct {
	ServerID    string   `json:"server_id"`
	DatabaseIDs []string `json:"database_ids"`
}

func (s *Server) handleMoveSite(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !readJSON(w, r, &req) {
		return
	}
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if site.State != "active" {
		writeError(w, http.StatusConflict, "site_not_active", "the website must be working before it can move; see its status first")
		return
	}
	if req.ServerID == site.ServerID {
		writeError(w, http.StatusBadRequest, "same_server", "the website is already on this server")
		return
	}
	target, err := s.store.GetServer(r.Context(), req.ServerID)
	if !isUUID(req.ServerID) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_server", "choose a server to move the website to")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.moveTargetOK(w, r, site.OwnerID, site.ServerID, target.ID) {
		return
	}
	// The databases must be on the website's server, belong to its owner, and have names
	// the target does not use yet.
	targetDBs, err := s.store.ListDatabases(r.Context(), target.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	m := store.Move{SiteID: &site.ID, FromServer: site.ServerID, ToServer: target.ID, LinuxUser: site.LinuxUser,
		DatabaseIDs: []string{}, Databases: []store.MoveDatabase{}, CreatedBy: &currentUser(r).ID}
	for _, id := range req.DatabaseIDs {
		if slices.Contains(m.DatabaseIDs, id) {
			continue
		}
		db, err := s.store.GetDatabase(r.Context(), id)
		if !isUUID(id) || errors.Is(err, store.ErrNotFound) || (err == nil && (db.ServerID != site.ServerID || !sameOwner(db.OwnerID, site.OwnerID))) {
			writeError(w, http.StatusBadRequest, "invalid_database", "a chosen database is not on the website's server or belongs to someone else")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if db.State != "active" {
			writeError(w, http.StatusConflict, "database_not_active", "the database "+db.Name+" must be working before it can move")
			return
		}
		if slices.ContainsFunc(targetDBs, func(t store.Database) bool {
			return t.Engine == db.Engine && (t.Name == db.Name || t.Username == db.Username)
		}) {
			writeError(w, http.StatusConflict, "database_name_taken", "the target server already has a database or user named like "+db.Name)
			return
		}
		m.DatabaseIDs = append(m.DatabaseIDs, db.ID)
		m.Databases = append(m.Databases, store.MoveDatabase{ID: db.ID, Engine: db.Engine, Name: db.Name, Username: db.Username})
	}
	m, err = s.store.CreateMove(r.Context(), m)
	if errors.Is(err, store.ErrMoveOpen) {
		writeError(w, http.StatusConflict, "move_open", "this website is already being moved, or its old copy has not been removed yet")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, err := s.opts.Tasks.QueueSiteMove(r.Context(), m.ID, &currentUser(r).ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.move_started", site.Domain, map[string]any{"site_id": site.ID, "move_id": m.ID,
		"from": site.ServerName, "to": target.Name, "databases": len(m.DatabaseIDs)})
	writeJSON(w, http.StatusAccepted, m)
}

func sameOwner(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func (s *Server) handleSiteMoves(w http.ResponseWriter, r *http.Request) {
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	list, err := s.store.SiteMoves(r.Context(), site.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleMoveCleanup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	m, err := s.store.GetMove(r.Context(), id)
	if !isUUID(id) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "move_not_found", "no such move")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if m.Phase != "moved" && m.Phase != "cleaning" {
		writeError(w, http.StatusConflict, "move_not_moved", "there is no old copy to remove")
		return
	}
	t, err := s.opts.Tasks.QueueMoveCleanup(r.Context(), m.ID, &currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "move.cleanup", m.FromName, map[string]any{"move_id": m.ID, "site_id": m.SiteID, "app_id": m.AppID, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}

// moveTargetOK checks that both servers are connected and that a customer's thing only goes
// to a server their package allows; it writes the error otherwise.
func (s *Server) moveTargetOK(w http.ResponseWriter, r *http.Request, owner *string, from, to string) bool {
	if owner != nil {
		c, err := s.store.GetCustomer(r.Context(), *owner)
		if err != nil {
			s.internalError(w, r, err)
			return false
		}
		if c.PackageID != nil {
			pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
			if err != nil {
				s.internalError(w, r, err)
				return false
			}
			if len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, to) {
				writeError(w, http.StatusConflict, "server_not_in_package", "the customer's package does not include this server; add it to the package first")
				return false
			}
		}
	}
	for _, id := range []string{from, to} {
		if online, _ := s.hub.Latest(id); !online {
			writeError(w, http.StatusConflict, "agent_offline", "both servers must be connected to move")
			return false
		}
	}
	return true
}

func (s *Server) handleMoveApp(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !readJSON(w, r, &req) {
		return
	}
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	switch {
	case app.State != "active":
		writeError(w, http.StatusConflict, "app_not_active", "the app must be working before it can move; see its status first")
		return
	case app.PreviewOf != nil || app.Previews:
		writeError(w, http.StatusConflict, "app_has_previews", "apps with pull request previews (and previews) cannot move; turn previews off first")
		return
	case req.ServerID == app.ServerID:
		writeError(w, http.StatusBadRequest, "same_server", "the app is already on this server")
		return
	}
	if previews, err := s.store.Previews(r.Context(), app.ID); err != nil {
		s.internalError(w, r, err)
		return
	} else if len(previews) > 0 {
		writeError(w, http.StatusConflict, "app_has_previews", "this app still has pull request previews; delete them first")
		return
	}
	target, err := s.store.GetServer(r.Context(), req.ServerID)
	if !isUUID(req.ServerID) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_server", "choose a server to move the app to")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.moveTargetOK(w, r, app.OwnerID, app.ServerID, target.ID) {
		return
	}
	others, err := s.store.ListApps(r.Context(), target.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if slices.ContainsFunc(others, func(a store.App) bool { return a.Name == app.Name }) {
		writeError(w, http.StatusConflict, "app_name_taken", "the target server already has an app named "+app.Name)
		return
	}
	m, err := s.store.CreateMove(r.Context(), store.Move{AppID: &app.ID, FromServer: app.ServerID, ToServer: target.ID,
		DatabaseIDs: []string{}, Databases: []store.MoveDatabase{}, CreatedBy: &currentUser(r).ID})
	if errors.Is(err, store.ErrMoveOpen) {
		writeError(w, http.StatusConflict, "move_open", "this app is already being moved, or its old copy has not been removed yet")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, err := s.opts.Tasks.QueueSiteMove(r.Context(), m.ID, &currentUser(r).ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "app.move_started", app.Name, map[string]any{"app_id": app.ID, "move_id": m.ID, "from": app.ServerName, "to": target.Name})
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) handleAppMoves(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	list, err := s.store.AppMoves(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}
