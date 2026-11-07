package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func TestCron(t *testing.T) {
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
	applied := make(chan []agentproto.CronSpec, 50)
	runs := make(chan string, 5)
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			applied <- in.Cron
			res := agentproto.StateApplyResult{}
			for _, s := range in.Sites {
				res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, c := range in.Cron {
				res.Cron = append(res.Cron, agentproto.AppResult{ID: c.ID, Generation: c.Generation, OK: true})
			}
			return res, nil
		case agentproto.OpCronRun:
			var in agentproto.CronParams
			json.Unmarshal(raw, &in)
			runs <- in.ID
			return struct{}{}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})
	lastCron := func() []agentproto.CronSpec {
		t.Helper()
		var got []agentproto.CronSpec
		deadline := time.After(5 * time.Second)
		for {
			select {
			case c := <-applied:
				got = c
				if len(applied) == 0 && len(got) > 0 {
					return got
				}
			case <-deadline:
				t.Fatal("no state.apply with cron jobs")
			}
		}
	}

	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Cron", "max_sites": 2, "max_cron_jobs": 1}, &pkg)
	var ca, cb struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "a@example.com", "name": "A", "package_id": pkg.ID}, &ca)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "b@example.com", "name": "B", "package_id": pkg.ID}, &cb)
	a := login(t, p.web.URL, "a@example.com", ca.Password)
	b := login(t, p.web.URL, "b@example.com", cb.Password)
	var site struct {
		ID        string `json:"id"`
		LinuxUser string `json:"linux_user"`
	}
	a.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "php", "php_version": "8.4"}, &site)

	var e errBody
	base := map[string]any{"site_id": site.ID, "schedule": "*/5 * * * *", "timezone": "Asia/Dhaka", "command": "php artisan schedule:run"}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for x, y := range base {
			m[x] = y
		}
		m[k] = v
		return m
	}
	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{with("schedule", "every day"), "invalid_schedule"},
		{with("timezone", "Mars/Olympus"), "invalid_timezone"},
		{with("command", ""), "invalid_command"},
		{with("timeout_seconds", 5), "invalid_timeout"},
		{with("site_id", ""), "invalid_cron_target"},
	} {
		if st := a.do("POST", "/api/v1/cron", c.body, &e); e.Code != c.code {
			t.Errorf("%v: %d %s, want %s", c.body, st, e.Code, c.code)
		}
	}
	var job struct {
		ID      string     `json:"id"`
		Name    string     `json:"name"`
		NextRun *time.Time `json:"next_run"`
		State   string     `json:"state"`
	}
	if st := a.do("POST", "/api/v1/cron", base, &job); st != http.StatusCreated || job.NextRun == nil || job.Name != "php artisan schedule:run" {
		t.Fatalf("create: %d %+v", st, job)
	}
	spec := lastCron()[0]
	if spec.User != site.LinuxUser || spec.Owner != ca.Customer.ID || spec.Timezone != "Asia/Dhaka" || !spec.Enabled || spec.TimeoutSeconds != 3600 {
		t.Fatalf("spec %+v", spec)
	}
	eventually(t, "job active", func() bool {
		a.do("GET", "/api/v1/cron/"+job.ID, nil, &job)
		return job.State == "active"
	})
	if st := a.do("POST", "/api/v1/cron", base, &e); st != http.StatusConflict || e.Code != "limit_reached_cron" {
		t.Errorf("second job: %d %s", st, e.Code)
	}

	// Another customer: nothing to see or touch, and no jobs on someone else's site.
	j := "/api/v1/cron/" + job.ID
	for _, c := range []struct{ method, path string }{{"GET", j}, {"PUT", j}, {"DELETE", j}, {"POST", j + "/run"}, {"GET", j + "/status"}} {
		if st := b.do(c.method, c.path, base, nil); st != http.StatusNotFound {
			t.Errorf("customer B %s %s: %d", c.method, c.path, st)
		}
	}
	if st := b.do("POST", "/api/v1/cron", base, &e); st != http.StatusNotFound {
		t.Errorf("job on another's site: %d %s", st, e.Code)
	}
	var list []struct{ ID string }
	b.do("GET", "/api/v1/cron", nil, &list)
	if len(list) != 0 {
		t.Errorf("customer B sees %d jobs", len(list))
	}

	var preview struct {
		Next []time.Time `json:"next"`
	}
	a.do("POST", "/api/v1/cron/preview", map[string]string{"schedule": "0 9 * * mon-fri", "timezone": "Asia/Dhaka"}, &preview)
	if len(preview.Next) != 5 || preview.Next[0].In(time.FixedZone("BDT", 6*3600)).Hour() != 9 {
		t.Errorf("preview %v", preview.Next)
	}

	if st := a.do("POST", j+"/run", nil, nil); st != http.StatusNoContent || <-runs != job.ID {
		t.Errorf("run now: %d", st)
	}
	off := with("enabled", false)
	a.do("PUT", j, off, nil)
	if spec := lastCron()[0]; spec.Enabled || spec.Generation != 2 {
		t.Errorf("disable sent %+v", spec)
	}
	// Deleting the site takes its jobs with it.
	a.do("DELETE", "/api/v1/sites/"+site.ID, nil, nil)
	if spec := lastCron()[0]; !spec.Deleted {
		t.Errorf("jobs of a deleted site: %+v", spec)
	}
}
