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
	// A customer's website may only go to a server their package allows.
	if site.OwnerID != nil {
		c, err := s.store.GetCustomer(r.Context(), *site.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if c.PackageID != nil {
			pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			if len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, target.ID) {
				writeError(w, http.StatusConflict, "server_not_in_package", "the customer's package does not include this server; add it to the package first")
				return
			}
		}
	}
	for _, id := range []string{site.ServerID, target.ID} {
		if online, _ := s.hub.Latest(id); !online {
			writeError(w, http.StatusConflict, "agent_offline", "both servers must be connected to move a website")
			return
		}
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
	s.audit(r, "site.move_cleanup", m.FromName, map[string]any{"move_id": m.ID, "site_id": m.SiteID, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
