package tasks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	offlineAfter  = 3 * time.Minute // brief reconnects (agent updates, restarts) stay quiet
	diskAlertAt   = 90.0            // percent
	diskAlertDone = 85.0            // the alert repeats only after the disk got this free again
)

// monitorLoop watches servers for going offline and for full disks. Each condition is
// remembered, so an alert goes out when it starts and when it ends, not every minute.
func (r *Runner) monitorLoop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var certsChecked time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if time.Since(certsChecked) > time.Hour {
			certsChecked = time.Now()
			r.checkCerts(ctx)
		}
		servers, err := r.store.ListServers(ctx)
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("list servers to monitor", "err", err)
			}
			continue
		}
		for _, s := range servers {
			if s.EnrolledAt == nil {
				continue
			}
			online, m := r.hub.Latest(s.ID)
			r.checkOnline(ctx, s.ID, s.Name, online, s.LastSeenAt)
			if online && m != nil && m.DiskTotal > 0 {
				r.checkDisk(ctx, s.ID, s.Name, float64(m.DiskUsed)/float64(m.DiskTotal)*100)
			}
		}
	}
}

// Server state values: "online", "offline:<unix time first seen offline>" or "alerted".
func (r *Runner) checkOnline(ctx context.Context, id, name string, online bool, lastSeen *time.Time) {
	key := "server:" + id
	if online {
		if prev, _ := r.store.SwapAlertState(ctx, key, "online"); prev == "alerted" {
			r.alert(ctx, nil, "server_online", "info", map[string]string{"server": name}, "/servers/"+id)
		}
		return
	}
	since := time.Now()
	if lastSeen != nil {
		since = *lastSeen
	}
	prev, _ := r.store.SwapAlertState(ctx, key, "offline:"+strconv.FormatInt(since.Unix(), 10))
	switch {
	case prev == "alerted":
		r.store.SwapAlertState(ctx, key, "alerted")
	case strings.HasPrefix(prev, "offline:") || prev == "online":
		if time.Since(since) >= offlineAfter {
			r.store.SwapAlertState(ctx, key, "alerted")
			r.alert(ctx, nil, "server_offline", "critical",
				map[string]string{"server": name, "minutes": strconv.Itoa(int(time.Since(since).Minutes()))}, "/servers/"+id)
		}
	}
}

func (r *Runner) checkDisk(ctx context.Context, id, name string, percent float64) {
	key := "disk:" + id
	switch {
	case percent >= diskAlertAt:
		if prev, _ := r.store.SwapAlertState(ctx, key, "full"); prev != "full" {
			r.alert(ctx, nil, "server_disk", "warning", map[string]string{"server": name, "percent": fmt.Sprintf("%.0f", percent)}, "/servers/"+id)
		}
	case percent < diskAlertDone:
		r.store.SwapAlertState(ctx, key, "ok")
	}
}

// checkQuotas alerts customers (and the administrators) when their disk space is nearly or
// completely used up. It runs after disk use has been measured.
func (r *Runner) checkQuotas(ctx context.Context, serverID string) {
	sites, _ := r.store.ListSites(ctx, serverID)
	apps, _ := r.store.ListApps(ctx, serverID)
	owners := map[string]bool{}
	for _, s := range sites {
		if s.OwnerID != nil {
			owners[*s.OwnerID] = true
		}
	}
	for _, a := range apps {
		if a.OwnerID != nil {
			owners[*a.OwnerID] = true
		}
	}
	for id := range owners {
		c, err := r.store.GetCustomer(ctx, id)
		if err != nil {
			continue
		}
		pkg, err := r.store.GetPackage(ctx, *c.PackageID)
		if err != nil || pkg.DiskMB == nil {
			continue
		}
		limit := int64(*pkg.DiskMB) << 20
		level := "ok"
		switch {
		case c.Usage.DiskBytes >= limit:
			level = "full"
		case c.Usage.DiskBytes*10 >= limit*9:
			level = "warning"
		}
		prev, _ := r.store.SwapAlertState(ctx, "quota:"+id, level)
		if level != "ok" && level != prev {
			owner := id
			r.alert(ctx, &owner, "quota_"+level, map[string]string{"full": "critical", "warning": "warning"}[level],
				map[string]string{"customer": c.Name, "used": humanBytes(c.Usage.DiskBytes), "limit": humanBytes(limit)}, "/")
		}
	}
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// certWarning is how long before an uploaded certificate expires its owner hears about it.
const certWarning = 14 * 24 * time.Hour

// checkCerts warns once about each uploaded certificate that expires soon.
func (r *Runner) checkCerts(ctx context.Context) {
	sites, err := r.store.ExpiringCerts(ctx, time.Now().Add(certWarning))
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("list expiring certificates", "err", err)
		}
		return
	}
	for _, st := range sites {
		date := st.TLSExpiresAt.UTC().Format("2006-01-02")
		// The state names the expiry, so a new certificate that also expires soon warns again.
		if prev, _ := r.store.SwapAlertState(ctx, "cert:"+st.ID, date); prev != date {
			r.alert(ctx, st.OwnerID, "certificate_expiring", "warning", map[string]string{"name": st.Domain, "date": date}, "/sites/"+st.ID)
		}
	}
}
