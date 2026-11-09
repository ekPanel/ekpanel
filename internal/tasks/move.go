package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/hub"
	"ekpanel/internal/store"
)

// Moving a website to another server (PLAN.md, 4c): copy (source export → target import,
// passed on by the panel with flow control), switch (the site and its databases belong to
// the target, which is reconciled and gets the files and dumps), check, then the DNS
// follows. The old copy keeps serving until it is removed (KindMoveCleanup).
const (
	KindSiteMove    = "site.move"
	KindMoveCleanup = "site.move_cleanup"
)

// moveTimeout bounds one whole move; large sites take a while to copy.
const moveTimeout = 11 * time.Hour

func (r *Runner) QueueSiteMove(ctx context.Context, moveID string, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindSiteMove, Params: map[string]string{"move": moveID}, CreatedBy: by,
		MaxAttempts: 1, DedupeKey: "move:" + moveID})
}

// KindCustomerMove moves everything a customer has on one server to another, one thing at a
// time: the first website takes all their databases there along (websites reach their
// databases on the same server), then the other websites, then databases if they have no
// website there, then apps. It stops at the first failure; what moved stays moved.
const KindCustomerMove = "customer.move"

type CustomerMoveParams struct {
	Customer string `json:"customer"`
	From     string `json:"from"`
	To       string `json:"to"`
}

func (r *Runner) QueueCustomerMove(ctx context.Context, p CustomerMoveParams, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindCustomerMove, Params: p, CreatedBy: by, MaxAttempts: 1,
		DedupeKey: "customer-move:" + p.Customer})
}

// CustomerMoveResult says what moved.
type CustomerMoveResult struct {
	Moves []string `json:"moves"`
}

func (r *Runner) moveCustomer(ctx context.Context, t store.Task) (any, error) {
	var p CustomerMoveParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	sites, err := r.store.ListSites(ctx, p.From)
	if err != nil {
		return nil, err
	}
	dbs, err := r.store.ListDatabases(ctx, p.From)
	if err != nil {
		return nil, err
	}
	apps, err := r.store.ListApps(ctx, p.From)
	if err != nil {
		return nil, err
	}
	owned := func(o *string) bool { return o != nil && *o == p.Customer }
	var moves []store.Move
	var mdbs []store.MoveDatabase
	var dbIDs []string
	for _, d := range dbs {
		if owned(d.OwnerID) && d.State != "deleting" {
			mdbs = append(mdbs, store.MoveDatabase{ID: d.ID, Engine: d.Engine, Name: d.Name, Username: d.Username})
			dbIDs = append(dbIDs, d.ID)
		}
	}
	first := true
	for _, s := range sites {
		if !owned(s.OwnerID) || s.State == "deleting" {
			continue
		}
		m := store.Move{SiteID: &s.ID, FromServer: p.From, ToServer: p.To, LinuxUser: s.LinuxUser,
			DatabaseIDs: []string{}, Databases: []store.MoveDatabase{}, CreatedBy: t.CreatedBy}
		if first && len(mdbs) > 0 {
			m.DatabaseIDs, m.Databases = dbIDs, mdbs
		}
		first = false
		moves = append(moves, m)
	}
	if first { // no website: each database on its own
		for _, d := range mdbs {
			moves = append(moves, store.Move{FromServer: p.From, ToServer: p.To, DatabaseIDs: []string{d.ID},
				Databases: []store.MoveDatabase{d}, CreatedBy: t.CreatedBy})
		}
	}
	for _, a := range apps {
		if owned(a.OwnerID) && a.State != "deleting" && a.PreviewOf == nil {
			moves = append(moves, store.Move{AppID: &a.ID, FromServer: p.From, ToServer: p.To,
				DatabaseIDs: []string{}, Databases: []store.MoveDatabase{}, CreatedBy: t.CreatedBy})
		}
	}
	var res CustomerMoveResult
	for _, m := range moves {
		created, err := r.store.CreateMove(ctx, m)
		if err != nil {
			return res, fmt.Errorf("after %d of %d moved: %w", len(res.Moves), len(moves), err)
		}
		res.Moves = append(res.Moves, created.ID)
		if _, err := r.runMove(ctx, t, created.ID); err != nil {
			return res, fmt.Errorf("after %d of %d moved: %w", len(res.Moves)-1, len(moves), err)
		}
	}
	return res, nil
}

func (r *Runner) QueueMoveCleanup(ctx context.Context, moveID string, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindMoveCleanup, Params: map[string]string{"move": moveID}, CreatedBy: by,
		MaxAttempts: 3, DedupeKey: "move-cleanup:" + moveID})
}

func moveParam(t store.Task) (string, error) {
	var p struct {
		Move string `json:"move"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil || p.Move == "" {
		return "", errors.New("task has no move")
	}
	return p.Move, nil
}

func (r *Runner) moveSite(ctx context.Context, t store.Task) (any, error) {
	id, err := moveParam(t)
	if err != nil {
		return nil, err
	}
	return r.runMove(ctx, t, id)
}

// runMove runs one move (of a site, an app or databases) up to "moved".
func (r *Runner) runMove(ctx context.Context, t store.Task, id string) (any, error) {
	m, err := r.store.GetMove(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Phase != "copying" {
		return m, nil // already past this point (a repeated task)
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
	if m.AppID != nil {
		return r.moveApp(ctx, t, m)
	}
	if m.SiteID == nil {
		return r.moveDatabases(ctx, t, m)
	}
	site, err := r.store.GetSite(ctx, *m.SiteID)
	if err != nil {
		return nil, r.failMove(ctx, m, err)
	}

	// 1. Copy.
	res, err := r.copySite(ctx, m, site)
	if err != nil {
		r.discardCopy(m)
		return nil, r.failMove(ctx, m, fmt.Errorf("copy: %w", err))
	}
	r.store.SetMoveCopied(ctx, m.ID, res.Bytes)

	// 2. Switch: from here on, a failure gives the site back to the source.
	if err := r.store.SwitchMove(ctx, m); err != nil {
		r.discardCopy(m)
		return nil, r.failMove(ctx, m, err)
	}
	if err := r.finishSwitch(ctx, m, site); err != nil {
		r.rollBackMove(ctx, m)
		return nil, r.failMove(ctx, m, err)
	}

	// 3. Moved: the source keeps serving the old copy (without its cron jobs), and the DNS
	// records the panel manages follow the site.
	// The source first (it still counts the move as switching), so "moved" means both
	// servers are in their new state.
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Warn("reconcile the source after a move", "move", m.ID, "err", err)
	}
	if err := r.store.SetMovePhase(ctx, m.ID, "moved", nil); err != nil {
		return nil, err
	}
	if has, err := r.store.HasZones(ctx); err == nil && has {
		r.QueueDNSSync(ctx, "", t.CreatedBy)
	}
	r.QueueNameServers(ctx, t.CreatedBy)
	return r.store.GetMove(ctx, m.ID)
}

// finishSwitch makes the target serve the site: reconcile (account, PHP, web server,
// databases), put the files and dumps in place, and check the home page.
func (r *Runner) finishSwitch(ctx context.Context, m store.Move, site store.Site) error {
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		return fmt.Errorf("set up the site on the new server: %w", err)
	}
	s, err := r.store.GetSite(ctx, site.ID)
	if err != nil {
		return err
	}
	if s.State != "active" {
		msg := ""
		if s.Message != nil {
			msg = *s.Message
		}
		return fmt.Errorf("the new server could not set up the site: %s", msg)
	}
	p := agentproto.TransferFinishParams{Move: m.ID, User: site.LinuxUser, Domain: site.Domain}
	for _, d := range m.Databases {
		db, err := r.store.GetDatabase(ctx, d.ID)
		if err != nil {
			return err
		}
		if db.State != "active" {
			return fmt.Errorf("the new server could not set up the database %s", d.Name)
		}
		p.Databases = append(p.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name, User: d.Username})
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	raw, err := r.hub.Call(callCtx, m.ToServer, agentproto.OpTransferFinish, p)
	if err != nil {
		return fmt.Errorf("put the files in place: %w", err)
	}
	var fin agentproto.TransferFinishResult
	json.Unmarshal(raw, &fin)
	if fin.Status != 0 {
		r.store.SetMoveCheck(ctx, m.ID, fin.Status)
	}
	if fin.Status >= 500 {
		return fmt.Errorf("on the new server the site answered %d", fin.Status)
	}
	return nil
}

// copySite streams the site from the source to the target.
func (r *Runner) copySite(ctx context.Context, m store.Move, site store.Site) (agentproto.TransferResult, error) {
	exp := agentproto.TransferExportParams{User: site.LinuxUser, Domains: append([]string{site.Domain}, site.Aliases...)}
	for _, d := range m.Databases {
		exp.Databases = append(exp.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name})
	}
	return r.copyData(ctx, m, exp)
}

// copyData streams what exp names from the source to the target.
func (r *Runner) copyData(ctx context.Context, m store.Move, exp agentproto.TransferExportParams) (agentproto.TransferResult, error) {
	var res agentproto.TransferResult
	dst, err := r.hub.OpenStream(ctx, m.ToServer, agentproto.OpTransferImport, func(id string) any {
		return agentproto.TransferImportParams{Stream: id, Move: m.ID}
	})
	if err != nil {
		return res, fmt.Errorf("the new server: %w", err)
	}
	defer dst.Close()
	src, err := r.hub.OpenStream(ctx, m.FromServer, agentproto.OpTransferExport, func(id string) any {
		exp.Stream = id
		return exp
	})
	if err != nil {
		return res, fmt.Errorf("the current server: %w", err)
	}
	defer src.Close()
	return relay(ctx, src, dst)
}

// relay passes an export to an import with flow control: the source may have at most
// TransferWindow chunks on their way, and a chunk goes to the target only when the target
// has room for it.
func relay(ctx context.Context, src, dst hub.Stream) (agentproto.TransferResult, error) {
	var res agentproto.TransferResult
	if err := src.Credit(ctx, agentproto.TransferWindow); err != nil {
		return res, err
	}
	room := 0
	waitRoom := func() error {
		for room == 0 {
			select {
			case n := <-dst.Credits():
				room += n
			case _, ok := <-dst.Output():
				if !ok {
					return fmt.Errorf("the new server stopped: %s", dst.Reason())
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	for {
		var chunk []byte
		var ok bool
		select {
		case chunk, ok = <-src.Output():
		case <-ctx.Done():
			return res, ctx.Err()
		}
		if !ok {
			break
		}
		if err := waitRoom(); err != nil {
			return res, err
		}
		if err := dst.Write(ctx, chunk); err != nil {
			return res, err
		}
		room--
		if err := src.Credit(ctx, 1); err != nil {
			return res, err
		}
	}
	if reason := src.Reason(); reason != "done" {
		return res, fmt.Errorf("the current server stopped: %s", reason)
	}
	if err := dst.CloseWrite(ctx); err != nil {
		return res, err
	}
	for {
		select {
		case _, ok := <-dst.Output():
			if ok {
				continue
			}
			if reason := dst.Reason(); reason != "done" {
				return res, fmt.Errorf("the new server: %s", reason)
			}
			if err := json.Unmarshal(dst.Result(), &res); err != nil {
				return res, err
			}
			return res, nil
		case <-dst.Credits():
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
}

// discardCopy removes an unused copy on the target (best effort).
func (r *Runner) discardCopy(m store.Move) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := r.hub.Call(ctx, m.ToServer, agentproto.OpTransferDiscard, agentproto.TransferDiscardParams{Move: m.ID}); err != nil {
		r.log.Warn("discard the copy of a move", "move", m.ID, "err", err)
	}
}

// rollBackMove gives the site back to the source after a failed switch and removes the
// target's half-made copy.
func (r *Runner) rollBackMove(ctx context.Context, m store.Move) {
	ctx = context.WithoutCancel(ctx)
	if err := r.store.UnswitchMove(ctx, m); err != nil {
		r.log.Error("give a site back to its server", "move", m.ID, "err", err)
		return
	}
	r.store.SetTargetCleanup(ctx, m.ID, true)
	r.discardCopy(m)
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		r.log.Warn("remove the half-made copy of a move", "move", m.ID, "err", err)
	}
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Warn("reconcile the source after a failed move", "move", m.ID, "err", err)
	}
	// A DNS sync during the switch may already have pointed records at the target.
	if has, err := r.store.HasZones(ctx); err == nil && has {
		r.QueueDNSSync(ctx, "", nil)
	}
	r.QueueNameServers(ctx, nil)
}

func (r *Runner) failMove(ctx context.Context, m store.Move, cause error) error {
	msg := cause.Error()
	if err := r.store.SetMovePhase(context.WithoutCancel(ctx), m.ID, "failed", &msg); err != nil {
		r.log.Error("record a failed move", "move", m.ID, "err", err)
	}
	return cause
}

// cleanupMove removes the old copy on the source (the next apply there deletes it).
func (r *Runner) cleanupMove(ctx context.Context, t store.Task) (any, error) {
	id, err := moveParam(t)
	if err != nil {
		return nil, err
	}
	m, err := r.store.GetMove(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Phase == "done" {
		return m, nil
	}
	if m.Phase != "moved" && m.Phase != "cleaning" {
		return nil, fmt.Errorf("this move is %s, so there is no old copy to remove", m.Phase)
	}
	if err := r.store.SetMovePhase(ctx, m.ID, "cleaning", nil); err != nil {
		return nil, err
	}
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		return nil, err
	}
	if m, err = r.store.GetMove(ctx, id); err != nil {
		return nil, err
	}
	if m.Phase != "done" {
		msg := "the old copy was not removed"
		if m.Error != nil {
			msg = *m.Error
		}
		return nil, errors.New(msg)
	}
	return m, nil
}

// movesOn adds what moves leave on a server to its desired state: an old copy on the
// source is applied as it is (so it keeps serving) or, while being cleaned up, as deleted;
// the target's half-made copy after a failed switch is deleted. Returned: the moves by site
// ID, whose results must not be recorded as the site's own.
func (r *Runner) movesOn(ctx context.Context, serverID string, sites *[]store.Site) (map[string]store.Move, error) {
	moves, err := r.store.MovesOn(ctx, serverID)
	if err != nil {
		return nil, err
	}
	extra := map[string]store.Move{}
	for _, m := range moves {
		if m.SiteID == nil {
			continue
		}
		s, err := r.store.GetSite(ctx, *m.SiteID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.ServerID == serverID {
			continue // still this server's own (before the switch, or given back)
		}
		s.LinuxUser = m.LinuxUser
		extra[s.ID] = m
		*sites = append(*sites, s)
	}
	return extra, nil
}

// moveDeletes reports whether a move's copy on serverID is to be removed.
func moveDeletes(m store.Move, serverID string) bool {
	if m.FromServer == serverID {
		return m.Phase == "cleaning"
	}
	return m.TargetCleanup
}

// recordMoveCleanup notes on the move that its copy on serverID was removed (or why not).
func (r *Runner) recordMoveCleanup(ctx context.Context, m store.Move, serverID, failure string) {
	switch {
	case m.FromServer == serverID && failure == "":
		r.store.SetMovePhase(ctx, m.ID, "done", nil)
	case m.FromServer == serverID:
		msg := "removing the old copy: " + failure
		r.store.SetMovePhase(ctx, m.ID, "cleaning", &msg)
	case failure == "":
		r.store.SetTargetCleanup(ctx, m.ID, false)
	default:
		r.log.Warn("remove the half-made copy of a move", "move", m.ID, "err", failure)
	}
}

// RecoverMoves runs when the panel starts: a move that was copying or switching when the
// panel stopped can never finish, so it is failed (a switched site is given back).
func (r *Runner) RecoverMoves(ctx context.Context) {
	ids, err := r.store.InterruptedMoves(ctx)
	if err != nil {
		r.log.Error("find interrupted moves", "err", err)
		return
	}
	for _, id := range ids {
		m, err := r.store.GetMove(ctx, id)
		if err != nil {
			continue
		}
		if m.Phase == "switching" {
			r.rollBackMove(ctx, m)
		} else {
			r.discardCopy(m)
			// An app stopped for the copy starts again where it is.
			r.store.SetMovePhase(ctx, m.ID, "failed", nil)
			r.Reconcile(ctx, m.FromServer)
		}
		r.failMove(ctx, m, errors.New("the panel restarted during the move; it stays where it was"))
	}
}

// moveApp moves an app: stop it and copy its volumes (and a Git app's image), switch, set
// it up on the target, put the data in, start it there.
func (r *Runner) moveApp(ctx context.Context, t store.Task, m store.Move) (any, error) {
	app, err := r.store.GetApp(ctx, *m.AppID)
	if err != nil {
		return nil, r.failMove(ctx, m, err)
	}
	exp := agentproto.TransferExportParams{App: app.Name}
	if app.Domain != nil {
		exp.Domains = []string{*app.Domain}
	}
	if app.Kind == "git" && app.Image != nil {
		exp.Image = *app.Image
	}
	res, err := r.copyData(ctx, m, exp)
	if err != nil {
		r.discardCopy(m)
		err = r.failMove(ctx, m, fmt.Errorf("copy: %w", err))
		// The export stopped the app: start it again where it is.
		r.Reconcile(context.WithoutCancel(ctx), m.FromServer)
		return nil, err
	}
	r.store.SetMoveCopied(ctx, m.ID, res.Bytes)
	if err := r.store.SwitchMove(ctx, m); err != nil {
		r.discardCopy(m)
		err = r.failMove(ctx, m, err)
		r.Reconcile(context.WithoutCancel(ctx), m.FromServer)
		return nil, err
	}
	if err := r.finishAppSwitch(ctx, m, app); err != nil {
		r.rollBackMove(ctx, m)
		return nil, r.failMove(ctx, m, err)
	}
	// The source first (it still counts the move as switching), so "moved" means both
	// servers are in their new state.
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Warn("reconcile the source after a move", "move", m.ID, "err", err)
	}
	if err := r.store.SetMovePhase(ctx, m.ID, "moved", nil); err != nil {
		return nil, err
	}
	if has, err := r.store.HasZones(ctx); err == nil && has {
		r.QueueDNSSync(ctx, "", t.CreatedBy)
	}
	r.QueueNameServers(ctx, t.CreatedBy)
	return r.store.GetMove(ctx, m.ID)
}

func (r *Runner) appActive(ctx context.Context, id string) error {
	a, err := r.store.GetApp(ctx, id)
	if err != nil {
		return err
	}
	if a.State != "active" {
		msg := ""
		if a.Message != nil {
			msg = *a.Message
		}
		return fmt.Errorf("the new server could not run the app: %s", msg)
	}
	return nil
}

func (r *Runner) finishAppSwitch(ctx context.Context, m store.Move, app store.App) error {
	// Set up (and start) on empty volumes, then stop, fill them, and start again.
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		return fmt.Errorf("set up the app on the new server: %w", err)
	}
	if err := r.appActive(ctx, app.ID); err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	if _, err := r.hub.Call(callCtx, m.ToServer, agentproto.OpTransferFinish, agentproto.TransferFinishParams{
		Move: m.ID, App: app.Name, Stopped: app.Stopped,
	}); err != nil {
		return fmt.Errorf("put the data in place: %w", err)
	}
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		return fmt.Errorf("start the app on the new server: %w", err)
	}
	return r.appActive(ctx, app.ID)
}

// appMovesOn is movesOn for apps. It also returns the apps of this server that are being
// copied away: they must stay stopped, so their volumes do not change during the copy.
func (r *Runner) appMovesOn(ctx context.Context, serverID string, apps *[]store.App) (map[string]store.Move, map[string]bool, error) {
	moves, err := r.store.MovesOn(ctx, serverID)
	if err != nil {
		return nil, nil, err
	}
	extra, copying := map[string]store.Move{}, map[string]bool{}
	for _, m := range moves {
		if m.AppID == nil {
			continue
		}
		a, err := r.store.GetApp(ctx, *m.AppID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if a.ServerID == serverID {
			if m.Phase == "copying" {
				copying[a.ID] = true
			}
			continue
		}
		extra[a.ID] = m
		*apps = append(*apps, a)
	}
	return extra, copying, nil
}

// moveDatabases moves databases on their own: dump, switch, set them up on the target and
// load the dumps. The old copies stay until they are removed.
func (r *Runner) moveDatabases(ctx context.Context, t store.Task, m store.Move) (any, error) {
	exp := agentproto.TransferExportParams{}
	for _, d := range m.Databases {
		exp.Databases = append(exp.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name})
	}
	res, err := r.copyData(ctx, m, exp)
	if err != nil {
		r.discardCopy(m)
		return nil, r.failMove(ctx, m, fmt.Errorf("copy: %w", err))
	}
	r.store.SetMoveCopied(ctx, m.ID, res.Bytes)
	if err := r.store.SwitchMove(ctx, m); err != nil {
		r.discardCopy(m)
		return nil, r.failMove(ctx, m, err)
	}
	if err := r.finishDatabaseSwitch(ctx, m); err != nil {
		r.rollBackMove(ctx, m)
		return nil, r.failMove(ctx, m, err)
	}
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Warn("reconcile the source after a move", "move", m.ID, "err", err)
	}
	if err := r.store.SetMovePhase(ctx, m.ID, "moved", nil); err != nil {
		return nil, err
	}
	return r.store.GetMove(ctx, m.ID)
}

func (r *Runner) finishDatabaseSwitch(ctx context.Context, m store.Move) error {
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		return fmt.Errorf("set up the database on the new server: %w", err)
	}
	p := agentproto.TransferFinishParams{Move: m.ID}
	for _, d := range m.Databases {
		db, err := r.store.GetDatabase(ctx, d.ID)
		if err != nil {
			return err
		}
		if db.State != "active" {
			return fmt.Errorf("the new server could not set up the database %s", d.Name)
		}
		p.Databases = append(p.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name, User: d.Username})
	}
	callCtx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	if _, err := r.hub.Call(callCtx, m.ToServer, agentproto.OpTransferFinish, p); err != nil {
		return fmt.Errorf("load the data: %w", err)
	}
	return nil
}

// databaseMovesOn are the moves of databases on their own that concern serverID.
func (r *Runner) databaseMovesOn(ctx context.Context, serverID string) ([]store.Move, error) {
	moves, err := r.store.MovesOn(ctx, serverID)
	if err != nil {
		return nil, err
	}
	var out []store.Move
	for _, m := range moves {
		if m.SiteID == nil && m.AppID == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// KindSiteRebuild rebuilds a website (and chosen databases) on another server from a
// backup, for a server that is gone: no copy from the old server, which may be offline.
const KindSiteRebuild = "site.rebuild"

// RebuildParams: the move, and the snapshots to restore (by site or database ID).
type RebuildParams struct {
	Move      string            `json:"move"`
	Snapshots map[string]string `json:"snapshots"`
}

func (r *Runner) QueueSiteRebuild(ctx context.Context, p RebuildParams, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindSiteRebuild, Params: p, CreatedBy: by, MaxAttempts: 1, DedupeKey: "move:" + p.Move})
}

// LatestSnapshots lists the newest snapshot of each item in a repository, as the agent of
// its server sees it.
func (r *Runner) LatestSnapshots(ctx context.Context, repo store.BackupRepo) (map[string]agentproto.BackupSnapshot, error) {
	raw, err := r.hub.Call(ctx, repo.ServerID, agentproto.OpBackupSnapshots, agentproto.BackupSnapshotsParams{RepoID: repo.ID})
	if err != nil {
		return nil, err
	}
	var res agentproto.BackupSnapshotsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	latest := map[string]agentproto.BackupSnapshot{}
	for _, sn := range res.Snapshots {
		if old, ok := latest[sn.ItemID]; !ok || sn.Time.After(old.Time) {
			latest[sn.ItemID] = sn
		}
	}
	return latest, nil
}

func (r *Runner) rebuildSite(ctx context.Context, t store.Task) (any, error) {
	var p RebuildParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	m, err := r.store.GetMove(ctx, p.Move)
	if err != nil {
		return nil, err
	}
	if m.Phase != "copying" || m.SiteID == nil || m.FromBackup == nil {
		return m, nil
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
	site, err := r.store.GetSite(ctx, *m.SiteID)
	if err != nil {
		return nil, r.failMove(ctx, m, err)
	}
	if err := r.store.SwitchMove(ctx, m); err != nil {
		return nil, r.failMove(ctx, m, err)
	}
	if err := r.finishRebuild(ctx, m, site, p.Snapshots); err != nil {
		r.rollBackMove(ctx, m)
		return nil, r.failMove(ctx, m, err)
	}
	// The old server, if it is still there, keeps its copy until it is removed.
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Info("reconcile the old server after a rebuild", "move", m.ID, "err", err)
	}
	if err := r.store.SetMovePhase(ctx, m.ID, "moved", nil); err != nil {
		return nil, err
	}
	if has, err := r.store.HasZones(ctx); err == nil && has {
		r.QueueDNSSync(ctx, "", t.CreatedBy)
	}
	r.QueueNameServers(ctx, t.CreatedBy)
	return r.store.GetMove(ctx, m.ID)
}

// finishRebuild sets the site up on the target and restores its files and databases from
// the snapshots.
func (r *Runner) finishRebuild(ctx context.Context, m store.Move, site store.Site, snapshots map[string]string) error {
	if _, err := r.Reconcile(ctx, m.ToServer); err != nil {
		return fmt.Errorf("set up the site on the new server: %w", err)
	}
	if s, err := r.store.GetSite(ctx, site.ID); err != nil || s.State != "active" {
		return fmt.Errorf("the new server could not set up the site")
	}
	restore := func(p agentproto.BackupRestoreParams) error {
		callCtx, cancel := context.WithTimeout(ctx, 6*time.Hour)
		defer cancel()
		_, err := r.hub.Call(callCtx, m.ToServer, agentproto.OpBackupRestore, p)
		return err
	}
	if err := restore(agentproto.BackupRestoreParams{RepoID: *m.FromBackup, SnapshotID: snapshots[site.ID], Mode: "replace",
		Site: &agentproto.BackupSite{ID: site.ID, Name: site.Domain, User: site.LinuxUser}}); err != nil {
		return fmt.Errorf("restore the files: %w", err)
	}
	for _, d := range m.Databases {
		db, err := r.store.GetDatabase(ctx, d.ID)
		if err != nil {
			return err
		}
		if db.State != "active" {
			return fmt.Errorf("the new server could not set up the database %s", d.Name)
		}
		if err := restore(agentproto.BackupRestoreParams{RepoID: *m.FromBackup, SnapshotID: snapshots[d.ID],
			Database: &agentproto.BackupDatabase{ID: d.ID, Name: d.Name, Engine: d.Engine, Owner: d.Username}}); err != nil {
			return fmt.Errorf("restore the database %s: %w", d.Name, err)
		}
	}
	return nil
}
