package tasks

import (
	"context"
	"encoding/json"
	"time"

	"ekpanel/internal/agentproto"
)

const (
	trafficInterval = 15 * time.Minute
	trafficKeep     = 90 * 24 * time.Hour
)

// trafficLoop collects hourly request totals from every online server.
func (r *Runner) trafficLoop(ctx context.Context) {
	first := time.NewTimer(2 * time.Minute)
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	tick := time.NewTicker(trafficInterval)
	defer tick.Stop()
	for {
		ids, err := r.store.ServersWithSites(ctx)
		if err != nil && ctx.Err() == nil {
			r.log.Error("list servers for traffic", "err", err)
		}
		for _, id := range ids {
			if online, _ := r.hub.Latest(id); online {
				if err := r.CollectTraffic(ctx, id); err != nil && ctx.Err() == nil {
					r.log.Warn("collect traffic", "server", id, "err", err)
				}
			}
		}
		if err := r.store.PruneTraffic(ctx, trafficKeep); err != nil && ctx.Err() == nil {
			r.log.Warn("prune traffic", "err", err)
		}
		r.CheckBandwidth(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// CollectTraffic fetches the last hours of request totals from one server. Hours already
// stored are replaced, so overlapping calls are harmless.
func (r *Runner) CollectTraffic(ctx context.Context, serverID string) error {
	sites, err := r.store.ListSites(ctx, serverID)
	if err != nil {
		return err
	}
	apps, err := r.store.ListApps(ctx, serverID)
	if err != nil {
		return err
	}
	p := agentproto.TrafficParams{Since: time.Now().Add(-4 * time.Hour)}
	for _, s := range sites {
		if s.State != "deleting" {
			p.Sites = append(p.Sites, s.ID)
		}
	}
	for _, a := range apps {
		if a.State != "deleting" && a.Domain != nil {
			p.Apps = append(p.Apps, a.ID)
		}
	}
	if len(p.Sites)+len(p.Apps) == 0 {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, serverID, agentproto.OpTraffic, p)
	if err != nil {
		return err
	}
	var res agentproto.TrafficResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	return r.store.RecordTraffic(ctx, res.Buckets)
}
