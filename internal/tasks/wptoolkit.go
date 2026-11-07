package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

const (
	KindWordPressUpdate  = "site.wordpress_update"
	KindWordPressStaging = "site.wordpress_staging"
	KindWordPressCopy    = "site.wordpress_copy"
	KindWordPressScan    = "site.wordpress_scan"
)

// wordPressScanAge is how old a scan may get before the loop looks again; the loop also
// makes the automatic updates then.
const (
	wordPressScanAge      = 20 * time.Hour
	wordPressLoopInterval = 15 * time.Minute
	wordPressScanBatch    = 20
)

func wpSite(s store.Site) (agentproto.WordPressSite, error) {
	if s.Kind != "php" || s.PHPVersion == nil {
		return agentproto.WordPressSite{}, errors.New("WordPress needs a PHP site")
	}
	return agentproto.WordPressSite{User: s.LinuxUser, Domain: s.Domain, PHPVersion: *s.PHPVersion}, nil
}

// ScanWordPress reads a site's WordPress from its server and records it.
func (r *Runner) ScanWordPress(ctx context.Context, s store.Site) (agentproto.WordPressInfo, error) {
	var info agentproto.WordPressInfo
	site, err := wpSite(s)
	if err != nil {
		return info, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, s.ServerID, agentproto.OpWordPressInfo, agentproto.WordPressInfoParams{Site: site})
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, err
	}
	return info, r.store.SetWordPressInfo(ctx, s.ID, info)
}

// ManageWordPress makes one change on a site and records its new state.
func (r *Runner) ManageWordPress(ctx context.Context, s store.Site, action, name string) (agentproto.WordPressInfo, error) {
	var info agentproto.WordPressInfo
	site, err := wpSite(s)
	if err != nil {
		return info, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, s.ServerID, agentproto.OpWordPressManage, agentproto.WordPressManageParams{Site: site, Action: action, Name: name})
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return info, err
	}
	return info, r.store.SetWordPressInfo(ctx, s.ID, info)
}

// WordPressUpdate is what to update; Auto marks updates the loop started by itself.
type WordPressUpdate struct {
	SiteID  string   `json:"site_id"`
	Core    string   `json:"core,omitempty"`
	Plugins []string `json:"plugins,omitempty"`
	Themes  []string `json:"themes,omitempty"`
	Auto    bool     `json:"auto,omitempty"`
}

func (r *Runner) QueueWordPressUpdate(ctx context.Context, s store.Site, u WordPressUpdate, createdBy *string) (store.Task, error) {
	u.SiteID = s.ID
	return r.Enqueue(ctx, store.NewTask{Kind: KindWordPressUpdate, ServerID: &s.ServerID, Params: u, CreatedBy: createdBy,
		MaxAttempts: 1, DedupeKey: "wp-update:" + s.ID})
}

func (r *Runner) wordPressUpdate(ctx context.Context, t store.Task) (any, error) {
	var u WordPressUpdate
	if err := json.Unmarshal(t.Params, &u); err != nil {
		return nil, err
	}
	s, err := r.store.GetSite(ctx, u.SiteID)
	if err != nil {
		return nil, err
	}
	site, err := wpSite(s)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, lease-5*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, s.ServerID, agentproto.OpWordPressUpdate, agentproto.WordPressUpdateParams{
		Site: site, Core: u.Core, Plugins: u.Plugins, Themes: u.Themes})
	if err != nil {
		return nil, err
	}
	var res agentproto.WordPressUpdateResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if err := r.store.SetWordPressInfo(ctx, s.ID, res.Info); err != nil {
		return nil, err
	}
	var problems []string
	for _, it := range res.Items {
		if it.Outcome == "rolled_back" || it.Outcome == "failed" {
			problems = append(problems, it.Name+": "+it.Error)
		}
	}
	if len(problems) > 0 {
		r.alert(ctx, s.OwnerID, "wordpress_update", "warning", map[string]string{"name": s.Domain, "message": strings.Join(problems, "; ")}, "/sites/"+s.ID)
	}
	return res, nil
}

// QueueWordPressScan looks at the given sites again.
func (r *Runner) QueueWordPressScan(ctx context.Context, siteIDs []string, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindWordPressScan, Params: map[string][]string{"site_ids": siteIDs}, CreatedBy: createdBy, MaxAttempts: 1})
}

func (r *Runner) wordPressScan(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		SiteIDs []string `json:"site_ids"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	found, failed := 0, 0
	for _, id := range p.SiteIDs {
		s, err := r.store.GetSite(ctx, id)
		if err != nil || s.Kind != "php" || s.State == "deleting" {
			continue
		}
		info, err := r.ScanWordPress(ctx, s)
		switch {
		case err != nil:
			failed++
		case info.Installed:
			found++
		}
	}
	return map[string]int{"wordpress": found, "failed": failed}, nil
}

// wordPressLoop looks at every PHP site about once a day (finding WordPress installed by
// hand too) and makes the updates each site's rules ask for.
func (r *Runner) wordPressLoop(ctx context.Context) {
	tick := time.NewTicker(wordPressLoopInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		sites, err := r.store.WordPressDue(ctx, time.Now().Add(-wordPressScanAge), wordPressScanBatch)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("list WordPress sites", "err", err)
			}
			continue
		}
		for _, s := range sites {
			if online, _ := r.hub.Latest(s.ServerID); !online {
				continue
			}
			info, err := r.ScanWordPress(ctx, s)
			if err != nil {
				r.log.Warn("scan WordPress", "site", s.Domain, "err", err)
				continue
			}
			if u, ok := autoUpdates(s.WPAuto, info); ok {
				if _, err := r.QueueWordPressUpdate(ctx, s, u, nil); err != nil {
					r.log.Error("queue WordPress update", "site", s.Domain, "err", err)
				}
			}
		}
	}
}

// autoUpdates is what a site's rules want updated now, if anything.
func autoUpdates(rules store.WordPressAuto, info agentproto.WordPressInfo) (WordPressUpdate, bool) {
	u := WordPressUpdate{Auto: true}
	if !info.Installed || info.Error != "" {
		return u, false
	}
	switch {
	case rules.Core == "all" && info.CoreUpdate != "":
		u.Core = "latest"
	case (rules.Core == "minor" || rules.Core == "all") && info.CoreMinor != "":
		u.Core = "minor"
	}
	has := func(items []agentproto.WordPressItem) bool {
		for _, it := range items {
			if it.Update != "" {
				return true
			}
		}
		return false
	}
	if rules.Plugins && has(info.Plugins) {
		u.Plugins = []string{"*"}
	}
	if rules.Themes && has(info.Themes) {
		u.Themes = []string{"*"}
	}
	return u, u.Core != "" || len(u.Plugins) > 0 || len(u.Themes) > 0
}

// ---- Staging ----

// QueueWordPressStaging makes a staging copy of live in the (new, empty) site staging.
func (r *Runner) QueueWordPressStaging(ctx context.Context, live, staging store.Site, createdBy *string) (store.Task, error) {
	t, err := r.store.CreateTask(ctx, store.NewTask{Kind: KindWordPressStaging, ServerID: &live.ServerID,
		Params: map[string]string{"live_id": live.ID, "staging_id": staging.ID}, CreatedBy: createdBy, MaxAttempts: 1})
	if err != nil {
		return store.Task{}, err
	}
	r.vault.put(t.ID, map[string]string{"db": auth.NewDatabasePassword()})
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return t, nil
}

func (r *Runner) wordPressStaging(ctx context.Context, t store.Task) (any, error) {
	secrets, ok := r.vault.take(t.ID)
	if !ok {
		return nil, errors.New("the panel restarted before the copy began; delete the staging site and make it again")
	}
	var p struct {
		LiveID    string `json:"live_id"`
		StagingID string `json:"staging_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	live, err := r.store.GetSite(ctx, p.LiveID)
	if err != nil {
		return nil, err
	}
	staging, err := r.store.GetSite(ctx, p.StagingID)
	if err != nil {
		return nil, err
	}
	from, err := wpSite(live)
	if err != nil {
		return nil, err
	}
	to, err := wpSite(staging)
	if err != nil {
		return nil, err
	}
	// The copy's own database, like a WordPress install's.
	dbName := "wp_" + staging.LinuxUser
	hash := auth.MariaDBNativeHash(secrets["db"])
	db, err := r.store.CreateDatabase(ctx, store.NewDatabase{ServerID: staging.ServerID, Engine: "mariadb", Name: dbName, Username: dbName,
		PasswordHash: hash, CreatedBy: t.CreatedBy, OwnerID: staging.OwnerID})
	var taken store.ErrDatabaseNameTaken
	if errors.As(err, &taken) {
		if db, err = r.findDatabase(ctx, staging.ServerID, dbName); err == nil {
			db, err = r.store.SetDatabasePassword(ctx, db.ID, hash)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("prepare database: %w", err)
	}
	r.Reconcile(ctx, staging.ServerID)
	if staging, err = r.store.GetSite(ctx, staging.ID); err != nil {
		return nil, err
	}
	if db, err = r.store.GetDatabase(ctx, db.ID); err != nil {
		return nil, err
	}
	if staging.State != "active" {
		return nil, fmt.Errorf("the staging website is not ready: %s", deref(staging.Message, staging.State))
	}
	if db.State != "active" {
		return nil, fmt.Errorf("the staging database is not ready: %s", deref(db.Message, db.State))
	}
	res, err := r.clone(ctx, live.ServerID, agentproto.WordPressCloneParams{From: from, To: to, Files: true, Database: true, NoIndex: true,
		ToDB: &agentproto.CloneDB{Name: db.Name, User: db.Username, Password: secrets["db"]}})
	if err != nil {
		return nil, err
	}
	if err := r.store.SetWordPressInfo(ctx, staging.ID, res.Info); err != nil {
		return nil, err
	}
	return map[string]string{"staging": staging.Domain, "database": db.Name}, nil
}

func (r *Runner) clone(ctx context.Context, serverID string, p agentproto.WordPressCloneParams) (agentproto.WordPressCloneResult, error) {
	var res agentproto.WordPressCloneResult
	callCtx, cancel := context.WithTimeout(ctx, lease-5*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, serverID, agentproto.OpWordPressClone, p)
	if err != nil {
		return res, err
	}
	return res, json.Unmarshal(raw, &res)
}

// WordPressCopy copies between a staging site and its live site: "push" makes live like
// staging, "pull" refreshes staging from live.
type WordPressCopy struct {
	StagingID string `json:"staging_id"`
	Direction string `json:"direction"`
	Files     bool   `json:"files"`
	Database  bool   `json:"database"`
}

func (r *Runner) QueueWordPressCopy(ctx context.Context, staging store.Site, c WordPressCopy, createdBy *string) (store.Task, error) {
	c.StagingID = staging.ID
	return r.Enqueue(ctx, store.NewTask{Kind: KindWordPressCopy, ServerID: &staging.ServerID, Params: c, CreatedBy: createdBy,
		MaxAttempts: 1, DedupeKey: "wp-copy:" + staging.ID})
}

func (r *Runner) wordPressCopy(ctx context.Context, t store.Task) (any, error) {
	var c WordPressCopy
	if err := json.Unmarshal(t.Params, &c); err != nil {
		return nil, err
	}
	staging, err := r.store.GetSite(ctx, c.StagingID)
	if err != nil {
		return nil, err
	}
	if staging.StagingOf == nil {
		return nil, errors.New("this site is not a staging copy")
	}
	live, err := r.store.GetSite(ctx, *staging.StagingOf)
	if err != nil {
		return nil, err
	}
	a, err := wpSite(live)
	if err != nil {
		return nil, err
	}
	b, err := wpSite(staging)
	if err != nil {
		return nil, err
	}
	p := agentproto.WordPressCloneParams{From: b, To: a, Files: c.Files, Database: c.Database}
	target := live
	if c.Direction == "pull" {
		p = agentproto.WordPressCloneParams{From: a, To: b, Files: c.Files, Database: c.Database, NoIndex: true}
		target = staging
	}
	res, err := r.clone(ctx, live.ServerID, p)
	if err != nil {
		return nil, err
	}
	if err := r.store.SetWordPressInfo(ctx, target.ID, res.Info); err != nil {
		return nil, err
	}
	return map[string]string{"site": target.Domain, "backup": res.Backup}, nil
}
