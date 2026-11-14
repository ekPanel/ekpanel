package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

func TestSuspendBandwidthOwnerAndCronAlerts(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, cfg, _, _ := enrolled(t, p, admin, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var mu sync.Mutex
	var last agentproto.StateApplyParams
	cronResults := []agentproto.CronResult{}
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			mu.Lock()
			last = in
			mu.Unlock()
			res := agentproto.StateApplyResult{}
			for _, s := range in.Sites {
				res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, a := range in.Apps {
				res.Apps = append(res.Apps, agentproto.AppResult{ID: a.ID, Generation: a.Generation, OK: true})
			}
			for _, d := range in.Databases {
				res.Databases = append(res.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			for _, c := range in.Cron {
				res.Cron = append(res.Cron, agentproto.AppResult{ID: c.ID, Generation: c.Generation, OK: true})
			}
			return res, nil
		case agentproto.OpCronResults:
			mu.Lock()
			defer mu.Unlock()
			return agentproto.CronResultsResult{Jobs: cronResults}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})
	applied := func(what string, ok func(agentproto.StateApplyParams) bool) {
		t.Helper()
		eventually(t, what, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return ok(last)
		})
	}

	type idOnly struct {
		ID string `json:"id"`
	}
	var big, small idOnly
	if st := admin.do("POST", "/api/v1/packages", map[string]any{"name": "Big", "max_sites": 5, "max_databases": 5, "max_apps": 5,
		"max_cron_jobs": 5, "sftp": true, "bandwidth_gb": 1, "bandwidth_stop": true}, &big); st != http.StatusCreated {
		t.Fatalf("package: %d", st)
	}
	var e errBody
	if st := admin.do("POST", "/api/v1/packages", map[string]any{"name": "Bad", "bandwidth_gb": 0}, &e); st != http.StatusBadRequest {
		t.Errorf("0 GB bandwidth: %d %s", st, e.Code)
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Small", "max_sites": 1, "max_cron_jobs": 1}, &small)
	type created struct {
		Customer idOnly `json:"customer"`
		Password string `json:"password"`
	}
	var ca, cb created
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "a@example.com", "name": "A", "package_id": big.ID}, &ca)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "b@example.com", "name": "B", "package_id": small.ID}, &cb)
	a := login(t, p.web.URL, "a@example.com", ca.Password)

	var site, app idOnly
	var db struct {
		Database idOnly `json:"database"`
	}
	if st := a.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "static"}, &site); st != http.StatusCreated {
		t.Fatalf("site: %d", st)
	}
	a.do("POST", "/api/v1/sites/"+site.ID+"/sftp", map[string]bool{"enabled": true}, nil)
	if st := a.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "kind": "image", "image": "nginx:alpine",
		"container_port": 80, "domain": "app.example.com"}, &app); st != http.StatusCreated {
		t.Fatalf("app: %d", st)
	}
	a.do("POST", "/api/v1/databases", map[string]any{"server_id": serverID, "engine": "mariadb", "name": "shop"}, &db)
	var job idOnly
	if st := a.do("POST", "/api/v1/cron", map[string]any{"site_id": site.ID, "schedule": "*/5 * * * *", "timezone": "UTC", "command": "true"}, &job); st != http.StatusCreated {
		t.Fatalf("cron: %d", st)
	}
	applied("everything applied", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && len(in.Apps) == 1 && len(in.Cron) == 1 && in.Sites[0].SFTPEnabled && in.Cron[0].Enabled && in.Sites[0].Blocked == ""
	})

	// ---- Suspending ----
	if st := admin.do("POST", "/api/v1/customers/"+ca.Customer.ID+"/suspend", map[string]string{"reason": "unpaid invoice 42"}, nil); st != http.StatusOK {
		t.Fatalf("suspend: %d", st)
	}
	applied("suspended state", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && in.Sites[0].Blocked == agentproto.BlockedSuspended && !in.Sites[0].SFTPEnabled &&
			len(in.Apps) == 1 && in.Apps[0].Blocked == agentproto.BlockedSuspended && in.Apps[0].Stopped &&
			len(in.Cron) == 1 && !in.Cron[0].Enabled
	})
	if st := a.do("GET", "/api/v1/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("a suspended customer's session still works: %d", st)
	}
	c := newClient(t, p.web.URL)
	if st := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": ca.Password}, &e); st != http.StatusForbidden || e.Code != "account_suspended" {
		t.Errorf("suspended sign-in: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/auth/login", map[string]string{"email": "a@example.com", "password": "wrong password here"}, &e); e.Code != "invalid_credentials" {
		t.Errorf("a wrong password tells about the suspension: %d %s", st, e.Code)
	}
	var cust struct {
		SuspendedAt   *time.Time `json:"suspended_at"`
		SuspendReason *string    `json:"suspend_reason"`
	}
	admin.do("GET", "/api/v1/customers/"+ca.Customer.ID, nil, &cust)
	if cust.SuspendedAt == nil || cust.SuspendReason == nil || *cust.SuspendReason != "unpaid invoice 42" {
		t.Errorf("customer %+v", cust)
	}
	// An administrator can still look around as the customer.
	if st := admin.do("POST", "/api/v1/customers/"+ca.Customer.ID+"/impersonate", nil, nil); st != http.StatusOK {
		t.Fatalf("impersonate a suspended customer: %d", st)
	}
	if st := admin.do("GET", "/api/v1/sites/"+site.ID, nil, nil); st != http.StatusOK {
		t.Errorf("as the suspended customer: %d", st)
	}
	admin.do("POST", "/api/v1/auth/impersonation/end", nil, nil)
	if st := admin.do("POST", "/api/v1/customers/"+ca.Customer.ID+"/unsuspend", nil, nil); st != http.StatusOK {
		t.Fatalf("unsuspend: %d", st)
	}
	applied("unsuspended state", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && in.Sites[0].Blocked == "" && in.Sites[0].SFTPEnabled && !in.Apps[0].Stopped && in.Cron[0].Enabled
	})
	a = login(t, p.web.URL, "a@example.com", ca.Password)

	// ---- Bandwidth ----
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour)
	if err := p.store.RecordTraffic(ctx, []agentproto.TrafficBucket{
		{Kind: "site", ID: site.ID, Hour: hour, Requests: 10, Bytes: 950 << 20, Paths: map[string]int64{"/": 10}},
	}); err != nil {
		t.Fatal(err)
	}
	p.runner.CheckBandwidth(ctx)
	if !hasNotification(t, a, "bandwidth_warning") {
		t.Error("no warning at 95%")
	}
	p.store.RecordTraffic(ctx, []agentproto.TrafficBucket{{Kind: "app", ID: app.ID, Hour: hour, Requests: 10, Bytes: 100 << 20, Paths: map[string]int64{"/": 10}}})
	p.runner.CheckBandwidth(ctx)
	if !hasNotification(t, a, "bandwidth_stopped") {
		t.Error("no notification when the bandwidth ran out")
	}
	applied("bandwidth block", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && in.Sites[0].Blocked == agentproto.BlockedBandwidth && in.Sites[0].SFTPEnabled &&
			in.Apps[0].Blocked == agentproto.BlockedBandwidth && !in.Apps[0].Stopped && in.Cron[0].Enabled
	})
	// A larger package lifts the block at once.
	admin.do("PUT", "/api/v1/packages/"+big.ID, map[string]any{"name": "Big", "max_sites": 5, "max_databases": 5, "max_apps": 5,
		"max_cron_jobs": 5, "sftp": true, "bandwidth_gb": 2, "bandwidth_stop": true}, nil)
	applied("bandwidth block lifted", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && in.Sites[0].Blocked == "" && in.Apps[0].Blocked == ""
	})

	// ---- Cron failures ----
	jobs, err := p.store.ListCronJobs(ctx, store.CronFilter{ServerID: serverID})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v %v", jobs, err)
	}
	setResult := func(result string) {
		mu.Lock()
		cronResults = []agentproto.CronResult{{ID: job.ID, Started: time.Now().Add(-time.Minute), Ended: time.Now(), Result: result, ExitCode: 3}}
		mu.Unlock()
	}
	cronAlerts := func() int {
		var out struct {
			Notifications []struct {
				Event string `json:"event"`
				Body  string `json:"body"`
			} `json:"notifications"`
		}
		a.do("GET", "/api/v1/notifications", nil, &out)
		n := 0
		for _, x := range out.Notifications {
			if x.Event == "cron_failed" {
				n++
			}
		}
		return n
	}
	setResult("exit-code")
	for range 2 {
		if err := p.runner.CheckCronResults(ctx, serverID, jobs); err != nil {
			t.Fatal(err)
		}
	}
	if n := cronAlerts(); n != 1 {
		t.Errorf("%d alerts for one failing job, want 1", n)
	}
	setResult("success")
	p.runner.CheckCronResults(ctx, serverID, jobs)
	setResult("timeout")
	p.runner.CheckCronResults(ctx, serverID, jobs)
	if n := cronAlerts(); n != 2 {
		t.Errorf("%d alerts after a success and a new failure, want 2", n)
	}

	// ---- Changing the owner ----
	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{map[string]any{"to": cb.Customer.ID}, "nothing_to_give"},
		{map[string]any{"to": cb.Customer.ID, "sites": []string{"nope"}}, "invalid_params"},
		{map[string]any{"to": cb.Customer.ID, "apps": []string{app.ID}}, "new_owner_limit_app"},
		{map[string]any{"to": cb.Customer.ID, "sites": []string{site.ID}, "databases": []string{db.Database.ID}}, "new_owner_limit_database"},
		{map[string]any{"to": ca.Customer.ID, "sites": []string{site.ID}}, "nothing_to_give"},
		{map[string]any{"to": "00000000-0000-0000-0000-000000000001", "sites": []string{site.ID}}, "customer_not_found"},
	} {
		if st := admin.do("POST", "/api/v1/owner-changes", c.body, &e); e.Code != c.code {
			t.Errorf("%v: %d %s, want %s", c.body, st, e.Code, c.code)
		}
	}
	if st := a.do("POST", "/api/v1/owner-changes", map[string]any{"to": cb.Customer.ID, "sites": []string{site.ID}}, nil); st != http.StatusForbidden {
		t.Errorf("a customer gave away a website: %d", st)
	}
	if st := admin.do("POST", "/api/v1/owner-changes", map[string]any{"to": cb.Customer.ID, "sites": []string{site.ID}}, nil); st != http.StatusOK {
		t.Fatalf("give the site to B: %d", st)
	}
	if st := admin.do("POST", "/api/v1/owner-changes", map[string]any{"databases": []string{db.Database.ID}}, nil); st != http.StatusOK {
		t.Fatalf("give the database to the administrators: %d", st)
	}
	b := login(t, p.web.URL, "b@example.com", cb.Password)
	var list []idOnly
	b.do("GET", "/api/v1/sites", nil, &list)
	if len(list) != 1 || list[0].ID != site.ID {
		t.Errorf("B's sites %v", list)
	}
	b.do("GET", "/api/v1/cron", nil, &list)
	if len(list) != 1 {
		t.Errorf("the cron job did not go with the site: %v", list)
	}
	a.do("GET", "/api/v1/databases", nil, &list)
	if len(list) != 0 {
		t.Errorf("A still has the database: %v", list)
	}
	applied("the site runs as B", func(in agentproto.StateApplyParams) bool {
		return len(in.Sites) == 1 && in.Sites[0].Owner == cb.Customer.ID && len(in.Databases) == 1 &&
			slices.ContainsFunc(in.Owners, func(o agentproto.OwnerSpec) bool { return o.ID == cb.Customer.ID })
	})
	var audit []struct {
		Action string `json:"action"`
	}
	admin.do("GET", "/api/v1/audit", nil, &audit)
	for _, want := range []string{"customer.suspended", "customer.unsuspended", "owner.changed"} {
		if !slices.ContainsFunc(audit, func(x struct {
			Action string `json:"action"`
		}) bool {
			return x.Action == want
		}) {
			t.Errorf("audit log lacks %s", want)
		}
	}
}

func hasNotification(t *testing.T, c *client, event string) bool {
	t.Helper()
	var out struct {
		Notifications []struct {
			Event string `json:"event"`
		} `json:"notifications"`
	}
	c.do("GET", "/api/v1/notifications", nil, &out)
	for _, n := range out.Notifications {
		if n.Event == event {
			return true
		}
	}
	return false
}
