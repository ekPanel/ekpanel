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
	m, err := r.store.GetMove(ctx, id)
	if err != nil {
		return nil, err
	}
	if m.Phase != "copying" || m.SiteID == nil {
		return m, nil // already past this point (a repeated task)
	}
	ctx, cancel := context.WithTimeout(ctx, moveTimeout)
	defer cancel()
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
	if err := r.store.SetMovePhase(ctx, m.ID, "moved", nil); err != nil {
		return nil, err
	}
	if _, err := r.Reconcile(ctx, m.FromServer); err != nil {
		r.log.Warn("reconcile the source after a move", "move", m.ID, "err", err)
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
	var res agentproto.TransferResult
	exp := agentproto.TransferExportParams{User: site.LinuxUser, Domains: append([]string{site.Domain}, site.Aliases...)}
	for _, d := range m.Databases {
		exp.Databases = append(exp.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name})
	}
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
func (r *Runner) recordMoveCleanup(ctx context.Context, moving map[string]store.Move, moveID, serverID, failure string) {
	var m store.Move
	for _, x := range moving {
		if x.ID == moveID {
			m = x
		}
	}
	if m.ID == "" {
		return
	}
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
