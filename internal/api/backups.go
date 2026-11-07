package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

var snapshotIDRE = regexp.MustCompile(`^[0-9a-f]{8,64}$`)

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func validSchedule(w http.ResponseWriter, sc store.BackupSchedule) bool {
	switch {
	case sc.Schedule != "off" && sc.Schedule != "hourly" && sc.Schedule != "daily" && sc.Schedule != "weekly":
		writeError(w, http.StatusBadRequest, "invalid_schedule", "choose off, hourly, daily or weekly")
	case sc.HourUTC < 0 || sc.HourUTC > 23:
		writeError(w, http.StatusBadRequest, "invalid_schedule", "the hour must be 0–23")
	case sc.KeepDaily < 0 || sc.KeepDaily > 365 || sc.KeepWeekly < 0 || sc.KeepWeekly > 520 || sc.KeepMonthly < 0 || sc.KeepMonthly > 240:
		writeError(w, http.StatusBadRequest, "invalid_retention", "keep between 0 and 365 daily, 520 weekly and 240 monthly backups")
	case sc.KeepDaily+sc.KeepWeekly+sc.KeepMonthly == 0:
		writeError(w, http.StatusBadRequest, "invalid_retention", "keep at least one backup")
	default:
		return true
	}
	return false
}

type createBackupRepoRequest struct {
	ServerID    string               `json:"server_id"`
	Name        string               `json:"name"`
	Kind        string               `json:"kind"`
	S3          *agentproto.BackupS3 `json:"s3"`
	RecoveryKey string               `json:"recovery_key"`
	store.BackupSchedule
}

type backupRepoCreated struct {
	Repo        store.BackupRepo `json:"repo"`
	RecoveryKey string           `json:"recovery_key,omitempty"`
}

func (s *Server) handleListBackupRepos(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListBackupRepos(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateBackupRepo sets the repository up on the server right away, because the
// recovery key it creates must go straight back to the user and nowhere else.
func (s *Server) handleCreateBackupRepo(w http.ResponseWriter, r *http.Request) {
	var req createBackupRepoRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "enter a name of up to 100 characters")
		return
	}
	if req.Kind != "local" && req.Kind != "s3" {
		writeError(w, http.StatusBadRequest, "invalid_backup_kind", "choose this server or S3")
		return
	}
	if !validSchedule(w, req.BackupSchedule) {
		return
	}
	if req.Kind == "local" && strings.TrimSpace(req.RecoveryKey) != "" {
		writeError(w, http.StatusBadRequest, "connect_needs_s3", "only backups in S3 storage can be connected again")
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	srv, err := s.store.GetServer(r.Context(), req.ServerID)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	id := newUUID()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute) // may install restic first
	defer cancel()
	raw, err := s.hub.Call(ctx, srv.ID, agentproto.OpBackupSetup, agentproto.BackupSetupParams{
		RepoID: id, Kind: req.Kind, S3: req.S3, RecoveryKey: strings.TrimSpace(req.RecoveryKey),
	})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	var res agentproto.BackupSetupResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.internalError(w, r, err)
		return
	}
	u := currentUser(r)
	repo, err := s.store.CreateBackupRepo(r.Context(), store.NewBackupRepo{
		ID: id, ServerID: srv.ID, Name: req.Name, Kind: req.Kind, Location: res.Location, Schedule: req.BackupSchedule, CreatedBy: &u.ID,
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "backup.repo_created", repo.Name, map[string]any{"repo_id": repo.ID, "server_id": srv.ID, "kind": repo.Kind,
		"connected_existing": req.RecoveryKey != ""})
	writeJSON(w, http.StatusCreated, backupRepoCreated{Repo: repo, RecoveryKey: res.RecoveryKey})
}

func backupRepoID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) loadBackupRepo(w http.ResponseWriter, r *http.Request) (store.BackupRepo, bool) {
	repo, err := s.store.GetBackupRepo(r.Context(), backupRepoID(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "backup_repo_not_found", "no backup location with that ID")
		return repo, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return repo, false
	}
	return repo, true
}

func (s *Server) handleGetBackupRepo(w http.ResponseWriter, r *http.Request) {
	if repo, ok := s.loadBackupRepo(w, r); ok {
		writeJSON(w, http.StatusOK, repo)
	}
}

func (s *Server) handleUpdateBackupRepo(w http.ResponseWriter, r *http.Request) {
	var sc store.BackupSchedule
	if !readJSON(w, r, &sc) || !validSchedule(w, sc) {
		return
	}
	repo, ok := s.loadBackupRepo(w, r)
	if !ok {
		return
	}
	repo, err := s.store.UpdateBackupSchedule(r.Context(), repo.ID, sc)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "backup.schedule_changed", repo.Name, map[string]any{"repo_id": repo.ID, "schedule": sc.Schedule})
	writeJSON(w, http.StatusOK, repo)
}

// handleDeleteBackupRepo removes the location from the panel and the server's settings for
// it. The backups themselves stay in place and can be connected again with the recovery key.
func (s *Server) handleDeleteBackupRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadBackupRepo(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := s.hub.Call(ctx, repo.ServerID, agentproto.OpBackupRemove, agentproto.BackupRemoveParams{RepoID: repo.ID}); err != nil {
		s.log.Warn("remove backup settings on server", "repo", repo.ID, "err", err)
	}
	if err := s.store.DeleteBackupRepo(r.Context(), repo.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "backup.repo_removed", repo.Name, map[string]any{"repo_id": repo.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunBackup(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadBackupRepo(w, r)
	if !ok {
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueBackup(r.Context(), repo, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "backup.started", repo.Name, map[string]any{"repo_id": repo.ID, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleBackupSnapshots(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadBackupRepo(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	raw, err := s.hub.Call(ctx, repo.ServerID, agentproto.OpBackupSnapshots, agentproto.BackupSnapshotsParams{RepoID: repo.ID})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(raw)
}

func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	var req tasks.RestoreRequest
	if !readJSON(w, r, &req) {
		return
	}
	repo, ok := s.loadBackupRepo(w, r)
	if !ok {
		return
	}
	req.RepoID = repo.ID
	if !snapshotIDRE.MatchString(req.SnapshotID) {
		writeError(w, http.StatusBadRequest, "invalid_restore", "choose a backup to restore from")
		return
	}
	target := ""
	switch {
	case req.SiteID != "" && isUUID(req.SiteID):
		if req.Mode != "folder" && req.Mode != "replace" {
			writeError(w, http.StatusBadRequest, "invalid_restore_mode", "choose a new folder or replacing the current files")
			return
		}
		site, err := s.store.GetSite(r.Context(), req.SiteID)
		if err != nil || site.ServerID != repo.ServerID {
			writeError(w, http.StatusNotFound, "site_not_found", "that website is not on this backup's server")
			return
		}
		target = site.Domain
	case req.DatabaseID != "" && isUUID(req.DatabaseID):
		d, err := s.store.GetDatabase(r.Context(), req.DatabaseID)
		if err != nil || d.ServerID != repo.ServerID {
			writeError(w, http.StatusNotFound, "database_not_found", "that database is not on this backup's server")
			return
		}
		target = d.Name
	default:
		writeError(w, http.StatusBadRequest, "invalid_restore", "choose a website or database to restore")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueRestore(r.Context(), repo.ServerID, req, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "backup.restore_started", target, map[string]any{"repo_id": repo.ID, "snapshot_id": req.SnapshotID, "mode": req.Mode, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
