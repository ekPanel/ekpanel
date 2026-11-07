package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// QueueBackup starts a backup of everything on the repository's server.
func (r *Runner) QueueBackup(ctx context.Context, repo store.BackupRepo, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindBackupRun, ServerID: &repo.ServerID, Params: map[string]string{"repo_id": repo.ID, "name": repo.Name},
		CreatedBy: createdBy, MaxAttempts: 1, DedupeKey: "backup:" + repo.ID,
	})
}

// backupScheduler queues scheduled backups once a minute.
func (r *Runner) backupScheduler(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		due, err := r.store.DueBackupRepos(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("find due backups", "err", err)
			}
			continue
		}
		for _, repo := range due {
			if _, err := r.QueueBackup(ctx, repo, nil); err != nil {
				r.log.Error("queue scheduled backup", "repo", repo.ID, "err", err)
			}
		}
	}
}

func (r *Runner) runBackup(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		RepoID string `json:"repo_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	repo, err := r.store.GetBackupRepo(ctx, p.RepoID)
	if err != nil {
		return nil, err
	}
	params := agentproto.BackupRunParams{RepoID: repo.ID, KeepDaily: repo.KeepDaily, KeepWeekly: repo.KeepWeekly, KeepMonthly: repo.KeepMonthly}
	sites, err := r.store.ListSites(ctx, repo.ServerID)
	if err != nil {
		return nil, err
	}
	for _, s := range sites {
		if s.State != "deleting" {
			params.Sites = append(params.Sites, agentproto.BackupSite{ID: s.ID, Name: s.Domain, User: s.LinuxUser})
		}
	}
	dbs, err := r.store.ListDatabases(ctx, repo.ServerID)
	if err != nil {
		return nil, err
	}
	for _, d := range dbs {
		if d.State != "deleting" {
			params.Databases = append(params.Databases, agentproto.BackupDatabase{ID: d.ID, Name: d.Name, Engine: d.Engine, Owner: d.Username})
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, maxTaskDuration-time.Hour)
	defer cancel()
	raw, err := r.hub.Call(callCtx, repo.ServerID, agentproto.OpBackupRun, params)
	if err != nil {
		r.store.RecordBackupRun(context.WithoutCancel(ctx), repo.ID, "failed", err.Error())
		return nil, err
	}
	var res agentproto.BackupRunResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	var failures []string
	ok := 0
	for _, it := range res.Items {
		if it.OK {
			ok++
		} else {
			failures = append(failures, it.Name+": "+it.Error)
		}
	}
	if res.PruneError != "" {
		failures = append(failures, "removing old backups: "+res.PruneError)
	}
	status := "succeeded"
	switch {
	case len(res.Items) > 0 && ok == 0:
		status = "failed"
	case len(failures) > 0:
		status = "partial"
	}
	message := strings.Join(failures, "; ")
	if err := r.store.RecordBackupRun(ctx, repo.ID, status, message); err != nil {
		return nil, err
	}
	if status != "succeeded" {
		return nil, fmt.Errorf("%d of %d items backed up: %s", ok, len(res.Items), message)
	}
	return map[string]int{"items": ok}, nil
}

type RestoreRequest struct {
	RepoID     string `json:"repo_id"`
	SnapshotID string `json:"snapshot_id"`
	SiteID     string `json:"site_id,omitempty"`
	DatabaseID string `json:"database_id,omitempty"`
	Mode       string `json:"mode,omitempty"`
}

func (r *Runner) QueueRestore(ctx context.Context, serverID string, req RestoreRequest, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindBackupRestore, ServerID: &serverID, Params: req, CreatedBy: createdBy, MaxAttempts: 1})
}

func (r *Runner) restoreBackup(ctx context.Context, t store.Task) (any, error) {
	var req RestoreRequest
	if err := json.Unmarshal(t.Params, &req); err != nil {
		return nil, err
	}
	repo, err := r.store.GetBackupRepo(ctx, req.RepoID)
	if err != nil {
		return nil, err
	}
	p := agentproto.BackupRestoreParams{RepoID: repo.ID, SnapshotID: req.SnapshotID, Mode: req.Mode}
	switch {
	case req.SiteID != "":
		s, err := r.store.GetSite(ctx, req.SiteID)
		if err != nil {
			return nil, err
		}
		if s.ServerID != repo.ServerID {
			return nil, errors.New("the website is on another server")
		}
		p.Site = &agentproto.BackupSite{ID: s.ID, Name: s.Domain, User: s.LinuxUser}
	case req.DatabaseID != "":
		d, err := r.store.GetDatabase(ctx, req.DatabaseID)
		if err != nil {
			return nil, err
		}
		if d.ServerID != repo.ServerID {
			return nil, errors.New("the database is on another server")
		}
		p.Database = &agentproto.BackupDatabase{ID: d.ID, Name: d.Name, Engine: d.Engine, Owner: d.Username}
	default:
		return nil, errors.New("nothing to restore")
	}
	callCtx, cancel := context.WithTimeout(ctx, maxTaskDuration-time.Hour)
	defer cancel()
	raw, err := r.hub.Call(callCtx, repo.ServerID, agentproto.OpBackupRestore, p)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
