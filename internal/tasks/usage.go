package tasks

import (
	"context"
	"encoding/json"
	"time"

	"ekpanel/internal/agentproto"
)

const usageInterval = 30 * time.Minute

// usageLoop measures the disk use of every site and app, for the packages' disk limits.
// Databases report their size with every apply already.
func (r *Runner) usageLoop(ctx context.Context) {
	first := time.NewTimer(time.Minute) // let agents connect after a restart
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	tick := time.NewTicker(usageInterval)
	defer tick.Stop()
	for {
		ids, err := r.store.ServersWithSites(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Error("list servers for disk use", "err", err)
		}
		for _, id := range ids {
			if online, _ := r.hub.Latest(id); online {
				if err := r.MeasureDiskUse(ctx, id); err != nil && ctx.Err() == nil {
					r.log.Warn("measure disk use", "server", id, "err", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// MeasureDiskUse asks one server's agent for the disk use of its sites and apps.
func (r *Runner) MeasureDiskUse(ctx context.Context, serverID string) error {
	sites, err := r.store.ListSites(ctx, serverID)
	if err != nil {
		return err
	}
	apps, err := r.store.ListApps(ctx, serverID)
	if err != nil {
		return err
	}
	var p agentproto.DiskUsageParams
	for _, s := range sites {
		if s.State != "deleting" && s.AppliedGeneration > 0 {
			p.Sites = append(p.Sites, agentproto.DiskUsageSite{ID: s.ID, User: s.LinuxUser})
		}
	}
	for _, a := range apps {
		if a.State != "deleting" && a.AppliedGeneration > 0 {
			p.Apps = append(p.Apps, agentproto.DiskUsageApp{ID: a.ID, Name: a.Name})
		}
	}
	if len(p.Sites) == 0 && len(p.Apps) == 0 {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, serverID, agentproto.OpDiskUsage, p)
	if err != nil {
		return err
	}
	var res agentproto.DiskUsageResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return r.store.RecordDiskUse(ctx, res.Sites, res.Apps)
}
