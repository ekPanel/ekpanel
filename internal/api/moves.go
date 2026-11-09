package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
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
		if _, err := s.store.OpenDatabaseMove(r.Context(), db.ID); err == nil {
			writeError(w, http.StatusConflict, "move_open", "the database "+db.Name+" is already being moved")
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
	// Databases' old copies may still be in use on the old server: by the same owner's
	// websites and apps that stayed there (a customer move that stopped part-way, or a
	// database moved on its own). Removing them needs ?force=1.
	if len(m.Databases) > 0 && r.URL.Query().Get("force") != "1" {
		users, err := s.oldCopyUsers(r, m)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if len(users) > 0 {
			writeErrorData(w, http.StatusConflict, "old_copy_in_use",
				"websites or apps of the same owner are still on the old server and may use these databases: "+strings.Join(users, ", "),
				map[string]any{"users": users})
			return
		}
	}
	t, err := s.opts.Tasks.QueueMoveCleanup(r.Context(), m.ID, &currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "move.cleanup", m.FromName, map[string]any{"move_id": m.ID, "site_id": m.SiteID, "app_id": m.AppID,
		"database_ids": m.DatabaseIDs, "task_id": t.ID, "forced": r.URL.Query().Get("force") == "1"})
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

func (s *Server) handleMoveDatabase(w http.ResponseWriter, r *http.Request) {
	var req moveRequest
	if !readJSON(w, r, &req) {
		return
	}
	db, err := s.getDatabase(r, databaseID(r))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	if db.State != "active" {
		writeError(w, http.StatusConflict, "database_not_active", "the database must be working before it can move")
		return
	}
	if req.ServerID == db.ServerID {
		writeError(w, http.StatusBadRequest, "same_server", "the database is already on this server")
		return
	}
	target, err := s.store.GetServer(r.Context(), req.ServerID)
	if !isUUID(req.ServerID) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_server", "choose a server to move the database to")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.moveTargetOK(w, r, db.OwnerID, db.ServerID, target.ID) {
		return
	}
	others, err := s.store.ListDatabases(r.Context(), target.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if slices.ContainsFunc(others, func(t store.Database) bool {
		return t.Engine == db.Engine && (t.Name == db.Name || t.Username == db.Username)
	}) {
		writeError(w, http.StatusConflict, "database_name_taken", "the target server already has a database or user named like "+db.Name)
		return
	}
	if _, err := s.store.OpenDatabaseMove(r.Context(), db.ID); err == nil {
		writeError(w, http.StatusConflict, "move_open", "this database is already being moved, or its old copy has not been removed yet")
		return
	}
	m, err := s.store.CreateMove(r.Context(), store.Move{FromServer: db.ServerID, ToServer: target.ID,
		DatabaseIDs: []string{db.ID}, Databases: []store.MoveDatabase{{ID: db.ID, Engine: db.Engine, Name: db.Name, Username: db.Username}},
		CreatedBy: &currentUser(r).ID})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, err := s.opts.Tasks.QueueSiteMove(r.Context(), m.ID, &currentUser(r).ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "database.move_started", db.Name, map[string]any{"database_id": db.ID, "move_id": m.ID, "from": db.ServerName, "to": target.Name})
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) handleDatabaseMoves(w http.ResponseWriter, r *http.Request) {
	db, err := s.getDatabase(r, databaseID(r))
	if err != nil {
		s.writeDatabaseStoreError(w, r, err)
		return
	}
	list, err := s.store.DatabaseMoves(r.Context(), db.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// A site's moves list its databases too; show only this database's own moves here.
	own := []store.Move{}
	for _, m := range list {
		if m.SiteID == nil && m.AppID == nil {
			own = append(own, m)
		}
	}
	writeJSON(w, http.StatusOK, own)
}

type customerMoveRequest struct {
	FromServerID string `json:"from_server_id"`
	ServerID     string `json:"server_id"`
}

// handleMoveCustomer moves everything a customer has on one server to another (see
// tasks.KindCustomerMove). Everything is checked first, so a move does not stop halfway on
// something that was known before it started.
func (s *Server) handleMoveCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerMoveRequest
	if !readJSON(w, r, &req) {
		return
	}
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	if !isUUID(req.FromServerID) || !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "invalid_server", "choose the server to move from and the server to move to")
		return
	}
	if req.FromServerID == req.ServerID {
		writeError(w, http.StatusBadRequest, "same_server", "choose two different servers")
		return
	}
	for _, id := range []string{req.FromServerID, req.ServerID} {
		if _, err := s.store.GetServer(r.Context(), id); errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "invalid_server", "choose the server to move from and the server to move to")
			return
		} else if err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	if !s.moveTargetOK(w, r, &c.ID, req.FromServerID, req.ServerID) {
		return
	}
	ctx := r.Context()
	sites, err1 := s.store.ListSites(ctx, req.FromServerID)
	dbs, err2 := s.store.ListDatabases(ctx, req.FromServerID)
	apps, err3 := s.store.ListApps(ctx, req.FromServerID)
	targetDBs, err4 := s.store.ListDatabases(ctx, req.ServerID)
	targetApps, err5 := s.store.ListApps(ctx, req.ServerID)
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil {
		s.internalError(w, r, err)
		return
	}
	mine := func(o *string) bool { return o != nil && *o == c.ID }
	count := 0
	blocked := func(code, msg string) {
		writeError(w, http.StatusConflict, code, msg)
	}
	for _, x := range sites {
		if !mine(x.OwnerID) || x.State == "deleting" {
			continue
		}
		count++
		if x.State != "active" {
			blocked("site_not_active", "the website "+x.Domain+" must be working before it can move")
			return
		}
		if _, err := s.store.OpenMove(ctx, x.ID); err == nil {
			blocked("move_open", "the website "+x.Domain+" is already being moved, or its old copy is still on the other server")
			return
		}
	}
	for _, d := range dbs {
		if !mine(d.OwnerID) {
			continue
		}
		count++
		if d.State != "active" {
			blocked("database_not_active", "the database "+d.Name+" must be working before it can move")
			return
		}
		if slices.ContainsFunc(targetDBs, func(t store.Database) bool {
			return t.Engine == d.Engine && (t.Name == d.Name || t.Username == d.Username)
		}) {
			blocked("database_name_taken", "the target server already has a database or user named like "+d.Name)
			return
		}
		if _, err := s.store.OpenDatabaseMove(ctx, d.ID); err == nil {
			blocked("move_open", "the database "+d.Name+" is already being moved")
			return
		}
	}
	for _, a := range apps {
		if !mine(a.OwnerID) || a.PreviewOf != nil {
			continue
		}
		count++
		if a.State != "active" {
			blocked("app_not_active", "the app "+a.Name+" must be working before it can move")
			return
		}
		if previews, err := s.store.Previews(ctx, a.ID); err != nil {
			s.internalError(w, r, err)
			return
		} else if a.Previews || len(previews) > 0 {
			blocked("app_has_previews", "the app "+a.Name+" has pull request previews; turn them off and delete them first")
			return
		}
		if slices.ContainsFunc(targetApps, func(t store.App) bool { return t.Name == a.Name }) {
			blocked("app_name_taken", "the target server already has an app named "+a.Name)
			return
		}
		if _, err := s.store.OpenAppMove(ctx, a.ID); err == nil {
			blocked("move_open", "the app "+a.Name+" is already being moved")
			return
		}
	}
	if count == 0 {
		writeError(w, http.StatusBadRequest, "nothing_to_move", "the customer has nothing on that server")
		return
	}
	t, err := s.opts.Tasks.QueueCustomerMove(ctx, tasks.CustomerMoveParams{Customer: c.ID, From: req.FromServerID, To: req.ServerID}, &currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "customer.move_started", c.Email, map[string]any{"customer_id": c.ID, "from": req.FromServerID, "to": req.ServerID,
		"items": count, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}

// oldCopyUsers names the websites and apps on a move's old server that belong to the same
// owner as the moved databases, and so may still connect to their old copies.
func (s *Server) oldCopyUsers(r *http.Request, m store.Move) ([]string, error) {
	db, err := s.store.GetDatabase(r.Context(), m.Databases[0].ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sites, err := s.store.ListSites(r.Context(), m.FromServer)
	if err != nil {
		return nil, err
	}
	apps, err := s.store.ListApps(r.Context(), m.FromServer)
	if err != nil {
		return nil, err
	}
	var users []string
	for _, x := range sites {
		if x.State != "deleting" && sameOwner(x.OwnerID, db.OwnerID) {
			users = append(users, x.Domain)
		}
	}
	for _, a := range apps {
		if a.State != "deleting" && sameOwner(a.OwnerID, db.OwnerID) {
			users = append(users, a.Name)
		}
	}
	return users, nil
}

type rebuildRequest struct {
	ServerID    string   `json:"server_id"`
	RepoID      string   `json:"repo_id"`
	DatabaseIDs []string `json:"database_ids"`
}

// handleRebuildSite rebuilds a website (and chosen databases) on another server from a
// backup, for when its server is gone. The repository must be connected on the new server
// (the same S3 storage, with its recovery key); the newest snapshot of each is restored.
func (s *Server) handleRebuildSite(w http.ResponseWriter, r *http.Request) {
	var req rebuildRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if req.ServerID == site.ServerID {
		writeError(w, http.StatusBadRequest, "same_server", "the website is already on this server")
		return
	}
	target, err := s.store.GetServer(ctx, req.ServerID)
	if !isUUID(req.ServerID) || errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "invalid_server", "choose a server to rebuild the website on")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if online, _ := s.hub.Latest(target.ID); !online {
		writeError(w, http.StatusConflict, "agent_offline", "the new server must be connected")
		return
	}
	repo, err := s.store.GetBackupRepo(ctx, req.RepoID)
	if !isUUID(req.RepoID) || errors.Is(err, store.ErrNotFound) || (err == nil && repo.ServerID != target.ID) {
		writeError(w, http.StatusBadRequest, "invalid_backup_repo", "choose a backup storage connected on the new server")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if site.OwnerID != nil {
		c, err := s.store.GetCustomer(ctx, *site.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if c.PackageID != nil {
			if pkg, err := s.store.GetPackage(ctx, *c.PackageID); err == nil && len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, target.ID) {
				writeError(w, http.StatusConflict, "server_not_in_package", "the customer's package does not include this server; add it to the package first")
				return
			}
		}
	}
	if _, err := s.store.OpenMove(ctx, site.ID); err == nil {
		writeError(w, http.StatusConflict, "move_open", "this website is already being moved, or its old copy is still on the other server")
		return
	}
	targetDBs, err := s.store.ListDatabases(ctx, target.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	m := store.Move{SiteID: &site.ID, FromServer: site.ServerID, ToServer: target.ID, LinuxUser: site.LinuxUser,
		DatabaseIDs: []string{}, Databases: []store.MoveDatabase{}, FromBackup: &repo.ID, CreatedBy: &currentUser(r).ID}
	for _, id := range req.DatabaseIDs {
		if slices.Contains(m.DatabaseIDs, id) {
			continue
		}
		db, err := s.store.GetDatabase(ctx, id)
		if !isUUID(id) || errors.Is(err, store.ErrNotFound) || (err == nil && (db.ServerID != site.ServerID || !sameOwner(db.OwnerID, site.OwnerID))) {
			writeError(w, http.StatusBadRequest, "invalid_database", "a chosen database is not on the website's server or belongs to someone else")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
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
	// Every item needs a snapshot in this repository.
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	latest, err := s.opts.Tasks.LatestSnapshots(callCtx, repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backup_unreadable", "the new server could not read the backups: "+err.Error())
		return
	}
	snaps := map[string]string{}
	need := map[string]string{site.ID: site.Domain}
	for _, d := range m.Databases {
		need[d.ID] = d.Name
	}
	for id, name := range need {
		sn, ok := latest[id]
		if !ok {
			writeErrorData(w, http.StatusConflict, "no_snapshot", "this backup storage has no backup of "+name, map[string]any{"item": name})
			return
		}
		snaps[id] = sn.ID
	}
	m, err = s.store.CreateMove(ctx, m)
	if errors.Is(err, store.ErrMoveOpen) {
		writeError(w, http.StatusConflict, "move_open", "this website is already being moved")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, err := s.opts.Tasks.QueueSiteRebuild(ctx, tasks.RebuildParams{Move: m.ID, Snapshots: snaps}, &currentUser(r).ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.rebuild_started", site.Domain, map[string]any{"site_id": site.ID, "move_id": m.ID, "to": target.Name,
		"repo_id": repo.ID, "snapshots": snaps})
	writeJSON(w, http.StatusAccepted, m)
}
