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
)

func TestResellers(t *testing.T) {
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
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		if op != agentproto.OpStateApply {
			return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
		}
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		mu.Lock()
		last = in
		mu.Unlock()
		res := agentproto.StateApplyResult{}
		for _, s := range in.Sites {
			res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
		}
		for _, d := range in.Databases {
			res.Databases = append(res.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
		}
		return res, nil
	})
	blocked := func(what string, want map[string]string) {
		t.Helper()
		eventually(t, what, func() bool {
			mu.Lock()
			defer mu.Unlock()
			got := map[string]string{}
			for _, s := range last.Sites {
				got[s.Domain] = s.Blocked
			}
			for d, b := range want {
				if v, ok := got[d]; !ok || v != b {
					return false
				}
			}
			return true
		})
	}

	type idOnly struct {
		ID string `json:"id"`
	}
	var e errBody

	// ---- The administrator makes a reseller ----
	limits := map[string]any{"max_customers": 2, "max_sites": 2, "max_databases": 1, "max_apps": 0, "max_cron_jobs": 2,
		"max_mail_domains": 0, "max_mailboxes": 0, "bandwidth_gb": 1, "bandwidth_stop": true, "php_versions": []string{"8.3"},
		"server_ids": []string{serverID}, "sftp": true, "ftp": false, "git_apps": false}
	var made struct {
		Reseller idOnly `json:"reseller"`
		Password string `json:"password"`
	}
	if st := admin.do("POST", "/api/v1/resellers", map[string]any{"email": "r@example.com", "name": "Host BD", "limits": limits}, &made); st != http.StatusCreated {
		t.Fatalf("reseller: %d", st)
	}
	rid := made.Reseller.ID
	var adminPkg, rPkg idOnly
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_sites": 5, "max_databases": 5}, &adminPkg)
	type created struct {
		Customer idOnly `json:"customer"`
		Password string `json:"password"`
	}
	var x created
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "x@example.com", "name": "X", "package_id": adminPkg.ID}, &x)
	var xSite idOnly
	admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "x.example.com", "kind": "static", "owner_id": x.Customer.ID}, &xSite)

	r := login(t, p.web.URL, "r@example.com", made.Password)
	for _, path := range []string{"/api/v1/resellers", "/api/v1/servers/" + serverID, "/api/v1/audit", "/api/v1/backups"} {
		if st := r.do("GET", path, nil, nil); st != http.StatusForbidden {
			t.Errorf("reseller GET %s: %d", path, st)
		}
	}

	// ---- Packages within the reseller's limits ----
	pkg := map[string]any{"name": "Starter", "max_sites": 2, "max_databases": 1, "max_cron_jobs": 2, "bandwidth_gb": 1,
		"php_versions": []string{"8.3"}, "server_ids": []string{serverID}, "sftp": true}
	for field, bad := range map[string]any{"max_sites": 3, "ftp": true, "bandwidth_gb": nil, "php_versions": []string{}, "max_apps": 1} {
		try := map[string]any{}
		for k, v := range pkg {
			try[k] = v
		}
		try[field] = bad
		if st := r.do("POST", "/api/v1/packages", try, &e); st != http.StatusBadRequest || e.Code != "package_over_reseller" {
			t.Errorf("package with %s=%v: %d %s", field, bad, st, e.Code)
		}
	}
	if st := r.do("POST", "/api/v1/packages", pkg, &rPkg); st != http.StatusCreated {
		t.Fatalf("reseller package: %d", st)
	}
	// Images and the mailbox size matter only for packages with apps and mailboxes.
	limits["app_images"], limits["mailbox_quota_mb"] = []string{"nginx"}, 1024
	admin.do("PUT", "/api/v1/resellers/"+rid, map[string]any{"email": "r@example.com", "name": "Host BD", "limits": limits}, nil)
	pkg["name"] = "No apps"
	if st := r.do("POST", "/api/v1/packages", pkg, &e); st != http.StatusCreated {
		t.Errorf("package without apps or mailboxes under limited images: %d %s", st, e.Code)
	}
	pkg["name"], pkg["max_mailboxes"] = "Mail", 5
	if st := r.do("POST", "/api/v1/packages", pkg, &e); st != http.StatusBadRequest || e.Code != "package_over_reseller" {
		t.Errorf("package with mailboxes of any size: %d %s", st, e.Code)
	}
	var pkgs []idOnly
	r.do("GET", "/api/v1/packages", nil, &pkgs)
	if len(pkgs) != 2 || !slices.ContainsFunc(pkgs, func(p idOnly) bool { return p.ID == rPkg.ID }) {
		t.Errorf("reseller sees packages %v", pkgs)
	}
	if st := r.do("GET", "/api/v1/packages/"+adminPkg.ID, nil, nil); st != http.StatusNotFound {
		t.Errorf("reseller reads the administrators' package: %d", st)
	}

	// ---- Customers ----
	var c1, c2 created
	if st := r.do("POST", "/api/v1/customers", map[string]string{"email": "c1@example.com", "name": "C1", "package_id": adminPkg.ID}, &e); st != http.StatusBadRequest {
		t.Errorf("customer on an administrators' package: %d", st)
	}
	r.do("POST", "/api/v1/customers", map[string]string{"email": "c1@example.com", "name": "C1", "package_id": rPkg.ID}, &c1)
	r.do("POST", "/api/v1/customers", map[string]string{"email": "c2@example.com", "name": "C2", "package_id": rPkg.ID}, &c2)
	if st := r.do("POST", "/api/v1/customers", map[string]string{"email": "c3@example.com", "name": "C3", "package_id": rPkg.ID}, &e); st != http.StatusConflict || e.Code != "reseller_limit_customer" {
		t.Errorf("third customer: %d %s", st, e.Code)
	}
	var mine []struct {
		ID           string  `json:"id"`
		ResellerName *string `json:"reseller_name"`
	}
	r.do("GET", "/api/v1/customers", nil, &mine)
	if len(mine) != 2 {
		t.Errorf("reseller sees %d customers", len(mine))
	}
	admin.do("GET", "/api/v1/customers?reseller_id="+rid, nil, &mine)
	if len(mine) != 2 || mine[0].ResellerName == nil || *mine[0].ResellerName != "Host BD" {
		t.Errorf("administrator's list of the reseller's customers: %+v", mine)
	}
	if st := r.do("GET", "/api/v1/customers/"+x.Customer.ID, nil, nil); st != http.StatusNotFound {
		t.Errorf("reseller reads someone else's customer: %d", st)
	}

	// ---- Websites: the customer's package and the reseller's totals ----
	var s1, s2 idOnly
	if st := r.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "s1.example.com", "kind": "static"}, &e); st != http.StatusBadRequest || e.Code != "customer_required" {
		t.Errorf("reseller's own website: %d %s", st, e.Code)
	}
	if st := r.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "s1.example.com", "kind": "static", "owner_id": x.Customer.ID}, &e); st != http.StatusBadRequest || e.Code != "customer_not_found" {
		t.Errorf("website for someone else's customer: %d %s", st, e.Code)
	}
	if st := r.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "s1.example.com", "kind": "static", "owner_id": c1.Customer.ID}, &s1); st != http.StatusCreated {
		t.Fatalf("website for c1: %d", st)
	}
	cl1 := login(t, p.web.URL, "c1@example.com", c1.Password)
	if st := cl1.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "s2.example.com", "kind": "static"}, &s2); st != http.StatusCreated {
		t.Fatalf("c1's second website: %d", st)
	}
	cl2 := login(t, p.web.URL, "c2@example.com", c2.Password)
	if st := cl2.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "s3.example.com", "kind": "static"}, &e); st != http.StatusConflict || e.Code != "reseller_limit_site" {
		t.Errorf("c2's website over the reseller's total: %d %s", st, e.Code)
	}
	var sites []idOnly
	r.do("GET", "/api/v1/sites", nil, &sites)
	if len(sites) != 2 {
		t.Errorf("reseller sees %d websites", len(sites))
	}
	if st := r.do("GET", "/api/v1/sites/"+xSite.ID, nil, nil); st != http.StatusNotFound {
		t.Errorf("reseller reads someone else's website: %d", st)
	}
	var php struct {
		Versions []string `json:"versions"`
	}
	r.do("GET", "/api/v1/php-versions", nil, &php)
	if len(php.Versions) != 1 || php.Versions[0] != "8.3" {
		t.Errorf("reseller's PHP versions: %v", php.Versions)
	}

	// ---- Changing the owner between the reseller's customers only ----
	if st := r.do("POST", "/api/v1/owner-changes", map[string]any{"to": c2.Customer.ID, "sites": []string{s2.ID}}, nil); st != http.StatusOK {
		t.Errorf("owner change between the reseller's customers: %d", st)
	}
	if st := r.do("POST", "/api/v1/owner-changes", map[string]any{"to": x.Customer.ID, "sites": []string{s1.ID}}, &e); st != http.StatusBadRequest {
		t.Errorf("owner change to someone else's customer: %d %s", st, e.Code)
	}
	if st := r.do("POST", "/api/v1/owner-changes", map[string]any{"to": c2.Customer.ID, "sites": []string{xSite.ID}}, &e); st != http.StatusNotFound {
		t.Errorf("owner change of someone else's website: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/owner-changes", map[string]any{"to": c1.Customer.ID, "sites": []string{xSite.ID}}, &e); st != http.StatusConflict || e.Code != "reseller_limit_site" {
		t.Errorf("administrator gives a website over the reseller's total: %d %s", st, e.Code)
	}

	// ---- Suspending ----
	admin.do("POST", "/api/v1/customers/"+c1.Customer.ID+"/suspend", map[string]string{"reason": "abuse"}, nil)
	if st := r.do("POST", "/api/v1/customers/"+c1.Customer.ID+"/unsuspend", nil, &e); st != http.StatusForbidden || e.Code != "suspended_by_admin" {
		t.Errorf("reseller lifts an administrator's suspension: %d %s", st, e.Code)
	}
	admin.do("POST", "/api/v1/customers/"+c1.Customer.ID+"/unsuspend", nil, nil)
	if st := r.do("POST", "/api/v1/customers/"+c2.Customer.ID+"/suspend", map[string]string{"reason": "unpaid"}, nil); st != http.StatusOK {
		t.Errorf("reseller suspends their customer: %d", st)
	}
	if st := r.do("POST", "/api/v1/customers/"+c2.Customer.ID+"/unsuspend", nil, nil); st != http.StatusOK {
		t.Errorf("reseller lifts their own suspension: %d", st)
	}
	if st := admin.do("POST", "/api/v1/resellers/"+rid+"/suspend", map[string]string{"reason": "unpaid"}, nil); st != http.StatusOK {
		t.Fatalf("suspend reseller: %d", st)
	}
	blocked("reseller suspended", map[string]string{"s1.example.com": agentproto.BlockedSuspended, "s2.example.com": agentproto.BlockedSuspended,
		"x.example.com": ""})
	if st := r.do("GET", "/api/v1/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Errorf("suspended reseller still signed in: %d", st)
	}
	if st := newClient(t, p.web.URL).do("POST", "/api/v1/auth/login", map[string]string{"email": "c1@example.com", "password": c1.Password}, &e); st == http.StatusOK {
		t.Error("a suspended reseller's customer signed in")
	}
	admin.do("POST", "/api/v1/resellers/"+rid+"/unsuspend", nil, nil)
	blocked("reseller unsuspended", map[string]string{"s1.example.com": "", "s2.example.com": ""})
	r = login(t, p.web.URL, "r@example.com", made.Password)

	// ---- The reseller's bandwidth covers all their customers ----
	ctx := context.Background()
	hour := time.Now().UTC().Truncate(time.Hour)
	p.store.RecordTraffic(ctx, []agentproto.TrafficBucket{
		{Kind: "site", ID: s1.ID, Hour: hour, Requests: 10, Bytes: 600 << 20, Paths: map[string]int64{"/": 10}},
		{Kind: "site", ID: s2.ID, Hour: hour, Requests: 10, Bytes: 600 << 20, Paths: map[string]int64{"/": 10}},
	})
	p.runner.CheckBandwidth(ctx)
	blocked("reseller's bandwidth used up", map[string]string{"s1.example.com": agentproto.BlockedBandwidth,
		"s2.example.com": agentproto.BlockedBandwidth, "x.example.com": ""})
	if !hasNotification(t, r, "bandwidth_stopped") {
		t.Error("the reseller was not told their bandwidth is used up")
	}
	limits["bandwidth_gb"] = 5
	admin.do("PUT", "/api/v1/resellers/"+rid, map[string]any{"email": "r@example.com", "name": "Host BD", "limits": limits}, nil)
	blocked("reseller's bandwidth raised", map[string]string{"s1.example.com": "", "s2.example.com": ""})

	// ---- Signing in as a customer, and deleting ----
	if st := r.do("POST", "/api/v1/customers/"+c1.Customer.ID+"/impersonate", nil, nil); st != http.StatusOK {
		t.Fatalf("reseller signs in as their customer: %d", st)
	}
	var me struct {
		ID string `json:"id"`
	}
	r.do("GET", "/api/v1/auth/me", nil, &me)
	if me.ID != c1.Customer.ID {
		t.Errorf("signed in as %s", me.ID)
	}
	r.do("POST", "/api/v1/auth/impersonation/end", nil, &me)
	if me.ID != rid {
		t.Errorf("back as %s", me.ID)
	}
	if st := admin.do("DELETE", "/api/v1/resellers/"+rid, nil, &e); st != http.StatusConflict || e.Code != "reseller_has_customers" {
		t.Errorf("delete a reseller with customers: %d %s", st, e.Code)
	}
}
