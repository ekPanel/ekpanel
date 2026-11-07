// Package tasks runs background work stored in the tasks table: anything slow or anything
// that asks an agent to change a server. Users see each task's status in the panel.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/hub"
	"ekpanel/internal/notify"
	"ekpanel/internal/secretbox"
	"ekpanel/internal/store"
)

// Kinds of task. Each has a handler registered in New.
const (
	KindServiceRestart   = "service.restart"
	KindServerReconcile  = "server.reconcile"
	KindWordPressInstall = "site.install_wordpress"
	KindBackupRun        = "backup.run"
	KindBackupRestore    = "backup.restore"
)

// lease is how long a worker may hold a task before it is considered crashed.
// Handlers must finish (or time out) well within it; installing a PHP version is the
// slowest thing a reconcile does.
const lease = 30 * time.Minute

// maxTaskDuration bounds any single task; backups of large sites are the longest.
const maxTaskDuration = 12 * time.Hour

// driftCheckInterval is how often every online server is re-applied, which repairs
// anything changed or broken on the server since the last run.
const driftCheckInterval = 10 * time.Minute

type Handler func(ctx context.Context, t store.Task) (result any, err error)

type Runner struct {
	store    *store.Store
	hub      *hub.Hub
	log      *slog.Logger
	handlers map[string]Handler
	wake     chan struct{}
	vault    *vault
	notify   *notify.Notifier
	secrets  *secretbox.Box
}

// SetSecrets lets the runner open sealed secrets, such as Git apps' deploy keys.
func (r *Runner) SetSecrets(b *secretbox.Box) { r.secrets = b }

// SetNotifier lets the runner send notifications. Without one, events are only logged.
func (r *Runner) SetNotifier(n *notify.Notifier) { r.notify = n }

// alert tells the administrators (and the owner, for a customer's resource) about an event.
func (r *Runner) alert(ctx context.Context, owner *string, event, severity string, params map[string]string, path string) {
	if r.notify != nil {
		r.notify.Admins(ctx, owner, event, severity, params, path)
	}
}

func New(st *store.Store, h *hub.Hub, log *slog.Logger) *Runner {
	r := &Runner{store: st, hub: h, log: log, wake: make(chan struct{}, 1), handlers: map[string]Handler{}, vault: newVault()}
	r.handlers[KindServiceRestart] = r.serviceRestart
	r.handlers[KindServerReconcile] = r.serverReconcile
	r.handlers[KindWordPressInstall] = r.installWordPress
	r.handlers[KindBackupRun] = r.runBackup
	r.handlers[KindBackupRestore] = r.restoreBackup
	r.handlers[KindAppUpdate] = r.appUpdate
	r.handlers[KindAppDeploy] = r.appDeploy
	r.handlers[KindPHPExtension] = r.phpExtension
	return r
}

// Enqueue stores a task and wakes a worker. Kind must have a handler.
func (r *Runner) Enqueue(ctx context.Context, n store.NewTask) (store.Task, error) {
	if r.handlers[n.Kind] == nil {
		return store.Task{}, fmt.Errorf("no handler for task kind %q", n.Kind)
	}
	t, err := r.store.CreateTask(ctx, n)
	if err == nil {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return t, err
}

// Run starts the workers and housekeeping, and blocks until ctx ends and every running
// task has returned.
func (r *Runner) Run(ctx context.Context, workers int) {
	// Nothing is deploying yet, so a deploy still marked as running was cut off.
	if err := r.store.FailInterruptedDeployments(ctx); err != nil {
		r.log.Error("end interrupted deployments", "err", err)
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work(ctx)
		}()
	}
	wg.Add(5)
	go func() {
		defer wg.Done()
		r.driftLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		r.monitorLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		r.trafficLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		r.usageLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		r.backupScheduler(ctx)
	}()
	r.housekeeping(ctx)
	wg.Wait()
}

func (r *Runner) work(ctx context.Context) {
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		t, err := r.store.ClaimTask(ctx, lease)
		if errors.Is(err, store.ErrNotFound) {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-idle.C:
			}
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("claim task", "err", err)
				time.Sleep(time.Second)
			}
			continue
		}
		r.execute(ctx, t)
	}
}

func (r *Runner) execute(ctx context.Context, t store.Task) {
	// A task in progress gets to finish its bookkeeping even if the panel is stopping.
	// Handlers set tighter timeouts of their own; this only bounds a runaway.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxTaskDuration)
	defer cancel()
	log := r.log.With("task", t.ID, "kind", t.Kind)

	// Renew the lease while the task runs, so hours-long backups are not recovered as crashed.
	stopBeat := make(chan struct{})
	defer close(stopBeat)
	go func() {
		tick := time.NewTicker(lease / 3)
		defer tick.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-tick.C:
				if err := r.store.ExtendLease(runCtx, t.ID, lease); err != nil {
					log.Warn("extend task lease", "err", err)
				}
			}
		}
	}()

	result, err := func() (res any, err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("task crashed: %v", v)
			}
		}()
		handler := r.handlers[t.Kind]
		if handler == nil {
			return nil, fmt.Errorf("this panel version cannot run %q tasks", t.Kind)
		}
		return handler(runCtx, t)
	}()
	if err != nil {
		log.Warn("task failed", "attempt", t.Attempts, "err", err)
		backoff := time.Duration(t.Attempts*t.Attempts) * 10 * time.Second
		if err := r.store.FailTask(runCtx, t.ID, err.Error(), backoff); err != nil {
			log.Error("record task failure", "err", err)
		}
		// Applies and backups report their own, more specific alerts.
		if t.Attempts >= t.MaxAttempts && t.Kind != KindServerReconcile && t.Kind != KindBackupRun {
			var owner *string
			if t.CreatedBy != nil {
				if u, err := r.store.UserByID(runCtx, *t.CreatedBy); err == nil && u.Role == "customer" {
					owner = t.CreatedBy
				}
			}
			r.alert(runCtx, owner, "task_failed", "warning", map[string]string{"task": t.Kind, "message": err.Error()}, "/")
		}
		return
	}
	if err := r.store.FinishTask(runCtx, t.ID, result); err != nil {
		log.Error("record task result", "err", err)
	}
}

func (r *Runner) housekeeping(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if n, err := r.store.RecoverTasks(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("recover tasks", "err", err)
		} else if n > 0 {
			r.log.Warn("recovered interrupted tasks", "count", n)
		}
		if err := r.store.DeleteOldTasks(ctx, 30*24*time.Hour); err != nil && ctx.Err() == nil {
			r.log.Error("delete old tasks", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---- Handlers ----

func (r *Runner) serviceRestart(ctx context.Context, t store.Task) (any, error) {
	if t.ServerID == nil {
		return nil, errors.New("task has no server")
	}
	var p agentproto.ServicesRestartParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(ctx, *t.ServerID, agentproto.OpServicesRestart, p)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

// ---- Desired state ----

// QueueReconcile asks for a server's sites to be applied. Repeated calls before the task
// runs collapse into one.
func (r *Runner) QueueReconcile(ctx context.Context, serverID string, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindServerReconcile, ServerID: &serverID, CreatedBy: createdBy,
		MaxAttempts: 3, DedupeKey: "reconcile:" + serverID,
	})
}

func (r *Runner) serverReconcile(ctx context.Context, t store.Task) (any, error) {
	if t.ServerID == nil {
		return nil, errors.New("task has no server")
	}
	return r.Reconcile(ctx, *t.ServerID)
}

type ReconcileSummary struct {
	Applied int `json:"applied"`
	Failed  int `json:"failed"`
}

// Reconcile sends the full desired state of one server to its agent and records the
// result for every site. It is safe to run at any time; an unchanged server is a no-op.
func (r *Runner) Reconcile(ctx context.Context, serverID string) (ReconcileSummary, error) {
	var sum ReconcileSummary
	sites, err := r.store.ListSites(ctx, serverID)
	if err != nil {
		return sum, err
	}
	params := agentproto.StateApplyParams{Sites: make([]agentproto.SiteSpec, 0, len(sites))}
	// Each customer with something here gets a slice with their package's limits. Things
	// being deleted need no slice, so a customer's last delete also removes theirs.
	packages := map[string]*store.Package{}
	owner := func(id *string, state string) (string, *store.Package, error) {
		if id == nil || state == "deleting" {
			return "", nil, nil
		}
		if p, ok := packages[*id]; ok {
			return *id, p, nil
		}
		c, err := r.store.GetCustomer(ctx, *id)
		if err != nil {
			return "", nil, err
		}
		p, err := r.store.GetPackage(ctx, *c.PackageID)
		if err != nil {
			return "", nil, err
		}
		packages[*id] = &p
		return *id, &p, nil
	}
	for _, s := range sites {
		spec := agentproto.SiteSpec{
			ID: s.ID, Generation: s.Generation, Domain: s.Domain, Aliases: s.Aliases, Kind: s.Kind,
			TLS: s.TLS, User: s.LinuxUser, Deleted: s.State == "deleting", Web: s.Web,
		}
		if s.TLS == "custom" && s.TLSCert != nil && !spec.Deleted {
			if r.secrets == nil {
				return sum, errors.New("the panel cannot open certificate keys")
			}
			key, err := r.secrets.Open(s.TLSKey)
			if err != nil {
				return sum, fmt.Errorf("open the certificate key of %s: %w", s.Domain, err)
			}
			spec.CertPEM, spec.KeyPEM = *s.TLSCert, string(key)
		}
		if s.PHPVersion != nil {
			spec.PHPVersion = *s.PHPVersion
		}
		spec.SFTPEnabled, spec.SSHKeys = s.SFTPEnabled, s.SSHKeys
		if s.SFTPPasswordHash != nil {
			spec.SFTPPasswordHash = *s.SFTPPasswordHash
		}
		id, pkg, err := owner(s.OwnerID, s.State)
		if err != nil {
			return sum, err
		}
		spec.Owner = id
		if pkg != nil && !pkg.SFTP {
			spec.SFTPEnabled = false // the package no longer includes SFTP
		}
		params.Sites = append(params.Sites, spec)
	}
	dbs, err := r.store.ListDatabases(ctx, serverID)
	if err != nil {
		return sum, err
	}
	params.Databases = make([]agentproto.DatabaseSpec, 0, len(dbs))
	for _, d := range dbs {
		if _, _, err := owner(d.OwnerID, d.State); err != nil {
			return sum, err
		}
		params.Databases = append(params.Databases, agentproto.DatabaseSpec{
			ID: d.ID, Generation: d.Generation, Engine: d.Engine, Name: d.Name, User: d.Username,
			PasswordHash: d.PasswordHash, Deleted: d.State == "deleting",
		})
	}
	apps, err := r.store.ListApps(ctx, serverID)
	if err != nil {
		return sum, err
	}
	params.Apps = make([]agentproto.AppSpec, 0, len(apps))
	for _, a := range apps {
		id, _, err := owner(a.OwnerID, a.State)
		if err != nil {
			return sum, err
		}
		spec := AppSpec(a)
		spec.Owner = id
		if a.Kind == "git" && a.State != "deleting" {
			if spec.KeepImages, err = r.store.KeptImages(ctx, a.ID); err != nil {
				return sum, err
			}
		}
		params.Apps = append(params.Apps, spec)
	}
	jobs, err := r.store.ListCronJobs(ctx, store.CronFilter{ServerID: serverID})
	if err != nil {
		return sum, err
	}
	params.Cron = make([]agentproto.CronSpec, 0, len(jobs))
	for _, j := range jobs {
		deleted := j.State == "deleting" || j.TargetState == "deleting"
		state := j.State
		if deleted {
			state = "deleting"
		}
		id, _, err := owner(j.OwnerID, state)
		if err != nil {
			return sum, err
		}
		spec := agentproto.CronSpec{ID: j.ID, Generation: j.Generation, Owner: id, Schedule: j.Schedule, Timezone: j.Timezone,
			Command: j.Command, TimeoutSeconds: j.TimeoutSeconds, Enabled: j.Enabled, Deleted: deleted}
		if j.SiteID != nil && j.LinuxUser != nil {
			spec.User = *j.LinuxUser
		} else if j.Service != nil {
			spec.App, spec.Service = j.TargetName, *j.Service
		}
		params.Cron = append(params.Cron, spec)
	}
	params.Owners = make([]agentproto.OwnerSpec, 0, len(packages))
	for id, p := range packages {
		o := agentproto.OwnerSpec{ID: id}
		if p.MemoryMB != nil {
			o.MemoryMB = *p.MemoryMB
		}
		if p.CPUPercent != nil {
			o.CPUPercent = *p.CPUPercent
		}
		params.Owners = append(params.Owners, o)
	}
	fw, err := r.store.GetFirewall(ctx, serverID)
	if err != nil {
		return sum, err
	}
	if fw.Mode != "unmanaged" {
		params.Firewall = &agentproto.FirewallSpec{Enabled: fw.Mode == "on", Rules: make([]agentproto.FirewallRuleSpec, 0, len(fw.Rules))}
		for _, fr := range fw.Rules {
			spec := agentproto.FirewallRuleSpec{ID: fr.ID, Action: fr.Action, Protocol: fr.Protocol, Source: fr.Source}
			if fr.PortFrom != nil && fr.PortTo != nil {
				spec.PortFrom, spec.PortTo = *fr.PortFrom, *fr.PortTo
			}
			params.Firewall.Rules = append(params.Firewall.Rules, spec)
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, lease-5*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, serverID, agentproto.OpStateApply, params)
	var opErr *agentproto.OpError
	if errors.As(err, &opErr) {
		// The agent could not start applying (for example, it is not running as root).
		// Show the reason on the waiting sites; an offline agent just leaves them waiting.
		if ferr := r.store.FailUnappliedSites(ctx, serverID, opErr.Message); ferr != nil {
			r.log.Error("record apply failure", "server", serverID, "err", ferr)
		}
		if ferr := r.store.FailUnappliedDatabases(ctx, serverID, opErr.Message); ferr != nil {
			r.log.Error("record apply failure", "server", serverID, "err", ferr)
		}
		if ferr := r.store.FailUnappliedApps(ctx, serverID, opErr.Message); ferr != nil {
			r.log.Error("record apply failure", "server", serverID, "err", ferr)
		}
		if ferr := r.store.FailUnappliedCron(ctx, serverID, opErr.Message); ferr != nil {
			r.log.Error("record apply failure", "server", serverID, "err", ferr)
		}
	}
	if err != nil {
		return sum, err
	}
	var res agentproto.StateApplyResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return sum, err
	}
	// Alert once when something starts failing, not on every apply.
	wasError := map[string]bool{}
	for _, s := range sites {
		wasError[s.ID] = s.State == "error"
	}
	for _, d := range dbs {
		wasError[d.ID] = d.State == "error"
	}
	for _, a := range apps {
		wasError[a.ID] = a.State == "error"
	}
	for _, sr := range res.Sites {
		if !sr.OK && !wasError[sr.ID] {
			for _, s := range sites {
				if s.ID == sr.ID && s.State != "deleting" {
					r.alert(ctx, s.OwnerID, "site_error", "critical", map[string]string{"name": s.Domain, "message": sr.Error}, "/sites/"+s.ID)
				}
			}
		}
		if err := r.store.ApplySiteResult(ctx, sr.ID, sr.Generation, sr.OK, sr.Error); err != nil {
			return sum, err
		}
		if sr.OK {
			sum.Applied++
		} else {
			sum.Failed++
		}
	}
	for _, dr := range res.Databases {
		if !dr.OK && !wasError[dr.ID] {
			for _, d := range dbs {
				if d.ID == dr.ID && d.State != "deleting" {
					r.alert(ctx, d.OwnerID, "database_error", "critical", map[string]string{"name": d.Name, "message": dr.Error}, "/databases/"+d.ID)
				}
			}
		}
		if err := r.store.ApplyDatabaseResult(ctx, dr.ID, dr.Generation, dr.OK, dr.Error, dr.SizeBytes); err != nil {
			return sum, err
		}
		if dr.OK {
			sum.Applied++
		} else {
			sum.Failed++
		}
	}
	for _, ar := range res.Apps {
		if !ar.OK && !wasError[ar.ID] {
			for _, a := range apps {
				if a.ID == ar.ID && a.State != "deleting" {
					r.alert(ctx, a.OwnerID, "app_error", "critical", map[string]string{"name": a.Name, "message": ar.Error}, "/apps/"+a.ID)
				}
			}
		}
		if err := r.store.ApplyAppResult(ctx, ar.ID, ar.Generation, ar.OK, ar.Error); err != nil {
			return sum, err
		}
		if ar.OK {
			sum.Applied++
		} else {
			sum.Failed++
		}
	}
	for _, cr := range res.Cron {
		if err := r.store.ApplyCronResult(ctx, cr.ID, cr.Generation, cr.OK, cr.Error); err != nil {
			return sum, err
		}
		if cr.OK {
			sum.Applied++
		} else {
			sum.Failed++
		}
	}
	if res.Firewall != nil {
		st := store.FirewallStatus{Active: res.Firewall.Active, SSHPorts: res.Firewall.SSHPorts, Error: res.Firewall.Error, At: time.Now()}
		if err := r.store.RecordFirewallStatus(ctx, serverID, st); err != nil {
			return sum, err
		}
		if st.Error != "" {
			return sum, fmt.Errorf("firewall: %s", st.Error)
		}
	}
	if sum.Failed > 0 {
		return sum, fmt.Errorf("%d of %d items could not be applied; see each website, database or app for the reason",
			sum.Failed, len(res.Sites)+len(res.Databases)+len(res.Apps)+len(res.Cron))
	}
	return sum, nil
}

// driftLoop re-applies every online server with sites. These runs are not tasks: they
// are routine and would flood the task list; failures show on the sites themselves.
func (r *Runner) driftLoop(ctx context.Context) {
	tick := time.NewTicker(driftCheckInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		ids, err := r.store.ServersWithSites(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("list servers for drift check", "err", err)
			}
			continue
		}
		for _, id := range ids {
			if online, _ := r.hub.Latest(id); !online {
				continue
			}
			if _, err := r.Reconcile(ctx, id); err != nil && ctx.Err() == nil {
				r.log.Warn("drift check", "server", id, "err", err)
			}
		}
	}
}
