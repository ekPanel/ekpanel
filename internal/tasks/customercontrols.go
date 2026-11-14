package tasks

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// ---- Failed cron jobs ----

const cronWatchInterval = 5 * time.Minute

// cronWatchLoop reads the last run of every enabled cron job and alerts when one failed.
func (r *Runner) cronWatchLoop(ctx context.Context) {
	tick := time.NewTicker(cronWatchInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		jobs, err := r.store.ListCronJobs(ctx, store.CronFilter{})
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("list cron jobs to watch", "err", err)
			}
			continue
		}
		byServer := map[string][]store.CronJob{}
		for _, j := range jobs {
			if j.Enabled && j.State != "deleting" && j.TargetState != "deleting" && j.AppliedGeneration > 0 {
				byServer[j.ServerID] = append(byServer[j.ServerID], j)
			}
		}
		for id, list := range byServer {
			if online, _ := r.hub.Latest(id); online {
				if err := r.CheckCronResults(ctx, id, list); err != nil && ctx.Err() == nil {
					r.log.Warn("read cron results", "server", id, "err", err)
				}
			}
		}
	}
}

// CheckCronResults alerts once for each job whose last run failed, and again only after a
// run of it has succeeded in between.
func (r *Runner) CheckCronResults(ctx context.Context, serverID string, jobs []store.CronJob) error {
	p := agentproto.CronResultsParams{IDs: make([]string, 0, len(jobs))}
	byID := map[string]store.CronJob{}
	for _, j := range jobs {
		p.IDs = append(p.IDs, j.ID)
		byID[j.ID] = j
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, serverID, agentproto.OpCronResults, p)
	if err != nil {
		return err
	}
	var res agentproto.CronResultsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	for _, jr := range res.Jobs {
		j, ok := byID[jr.ID]
		if !ok {
			continue
		}
		value := "ok"
		if jr.Result != "success" {
			value = "failed"
		}
		prev, err := r.store.SwapAlertState(ctx, "cron:"+j.ID, value)
		if err != nil {
			return err
		}
		// A failure from before the panel watched the job (an upgrade) is not news.
		if value != "failed" || prev == "failed" || (prev == "" && time.Since(jr.Ended) > time.Hour) {
			continue
		}
		path := "/"
		if j.SiteID != nil {
			path = "/sites/" + *j.SiteID
		} else if j.AppID != nil {
			path = "/apps/" + *j.AppID
		}
		r.alert(ctx, j.OwnerID, "cron_failed", "warning", map[string]string{"name": j.Name, "target": j.TargetName,
			"result": cronResultText(jr)}, path)
	}
	return nil
}

func cronResultText(jr agentproto.CronResult) string {
	switch jr.Result {
	case "exit-code":
		return "exit code " + strconv.Itoa(jr.ExitCode)
	case "timeout":
		return "time limit reached"
	case "signal", "core-dump":
		return "killed"
	case "oom-kill":
		return "out of memory"
	}
	return jr.Result
}

// ---- Bandwidth ----

// CheckBandwidth compares each limited customer's traffic this month with their package,
// and each limited reseller's (all their customers together) with their limits: it alerts at
// 90% and 100%, and blocks (or unblocks) websites and apps where the package or the reseller
// says so. ids limits the check to those customers and resellers (a customer's reseller is
// checked with them); none means all of them.
func (r *Runner) CheckBandwidth(ctx context.Context, ids ...string) {
	var customers, resellers []string
	if len(ids) == 0 {
		var err error
		if customers, err = r.store.BandwidthCustomers(ctx); err != nil {
			r.log.Error("customers with bandwidth limits", "err", err)
			return
		}
		if resellers, err = r.store.BandwidthResellers(ctx); err != nil {
			r.log.Error("resellers with bandwidth limits", "err", err)
			return
		}
	}
	for _, id := range ids {
		u, err := r.store.UserByID(ctx, id)
		if err != nil {
			continue
		}
		switch {
		case u.Role == "reseller":
			resellers = append(resellers, id)
		case u.Role == "customer":
			customers = append(customers, id)
			if u.ResellerID != nil {
				resellers = append(resellers, *u.ResellerID)
			}
		}
	}
	now := time.Now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, id := range customers {
		c, err := r.store.GetCustomer(ctx, id)
		if err != nil {
			continue
		}
		pkg, err := r.store.GetPackage(ctx, *c.PackageID)
		if err != nil {
			continue
		}
		var used int64
		if pkg.BandwidthGB != nil {
			if used, err = r.store.OwnerBandwidth(ctx, id, month); err != nil {
				r.log.Error("bandwidth of customer", "customer", id, "err", err)
				continue
			}
		}
		if r.bandwidthLevel(ctx, id, c.Name, month, used, pkg.BandwidthGB, pkg.BandwidthStop) {
			r.ReapplyOwner(ctx, id)
		}
	}
	for _, id := range dedupe(resellers) {
		rs, err := r.store.GetReseller(ctx, id)
		if err != nil {
			continue
		}
		if r.bandwidthLevel(ctx, id, rs.Name, month, rs.BandwidthMonth, rs.Limits.BandwidthGB, rs.Limits.BandwidthStop) {
			list, err := r.store.CustomersOfReseller(ctx, id)
			if err != nil {
				r.log.Error("customers of reseller", "reseller", id, "err", err)
				continue
			}
			for _, c := range list {
				r.ReapplyOwner(ctx, c)
			}
		}
	}
}

// bandwidthLevel alerts the account (a customer or a reseller) when their use this month
// crosses 90% or 100% of limitGB, records whether their websites are blocked for it, and
// reports whether that changed.
func (r *Runner) bandwidthLevel(ctx context.Context, id, name string, month time.Time, used int64, limitGB *int, stop bool) bool {
	level := "ok"
	var limit int64
	if limitGB != nil {
		limit = int64(*limitGB) << 30
		switch {
		case used >= limit:
			level = "full"
		case used*10 >= limit*9:
			level = "warning"
		}
	}
	// The key holds the month, so each month starts afresh.
	prev, _ := r.store.SwapAlertState(ctx, "bandwidth:"+id+":"+month.Format("2006-01"), level)
	if level != "ok" && level != prev && !(level == "warning" && prev == "full") {
		owner := id
		event := "bandwidth_" + level
		if level == "full" && stop {
			event = "bandwidth_stopped"
		}
		r.alert(ctx, &owner, event, map[string]string{"full": "critical", "warning": "warning"}[level],
			map[string]string{"customer": name, "used": humanBytes(used), "limit": humanBytes(limit)}, "/")
	}
	changed, err := r.store.SetBandwidthBlocked(ctx, id, level == "full" && stop)
	if err != nil {
		r.log.Error("record bandwidth block", "account", id, "err", err)
		return false
	}
	return changed
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// ReapplyOwner queues a reconcile of every server where the customer has something.
func (r *Runner) ReapplyOwner(ctx context.Context, ownerID string) {
	ids, err := r.store.ServersOfOwner(ctx, ownerID)
	if err != nil {
		r.log.Error("servers of customer", "customer", ownerID, "err", err)
		return
	}
	for _, id := range ids {
		if _, err := r.QueueReconcile(ctx, id, nil); err != nil {
			r.log.Error("queue reconcile", "server", id, "err", err)
		}
	}
}
