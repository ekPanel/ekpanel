package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func TestServiceApps(t *testing.T) {
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
	specs := map[string]agentproto.AppSpec{} // by name, as last applied
	var builds []agentproto.AppBuildParams
	restarted := map[string]int{}
	buildOK := true
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case agentproto.OpStateApply:
			var in agentproto.StateApplyParams
			json.Unmarshal(raw, &in)
			res := agentproto.StateApplyResult{}
			for _, s := range in.Sites {
				res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, a := range in.Apps {
				specs[a.Name] = a
				res.Apps = append(res.Apps, agentproto.AppResult{ID: a.ID, Generation: a.Generation, OK: true})
			}
			return res, nil
		case agentproto.OpServiceBuild:
			var in agentproto.AppBuildParams
			json.Unmarshal(raw, &in)
			builds = append(builds, in)
			return agentproto.AppBuildResult{OK: buildOK, Log: "pnpm build\nok\n"}, nil
		case agentproto.OpAppRestart:
			var in agentproto.AppParams
			json.Unmarshal(raw, &in)
			restarted[in.Name]++
			return struct{}{}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})
	spec := func(name string) agentproto.AppSpec {
		mu.Lock()
		defer mu.Unlock()
		return specs[name]
	}

	type idOnly struct {
		ID string `json:"id"`
	}
	type siteView struct {
		ID        string `json:"id"`
		LinuxUser string `json:"linux_user"`
		State     string `json:"state"`
	}
	var site, other siteView
	if st := admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "license.example.com", "kind": "static"}, &site); st != http.StatusCreated {
		t.Fatalf("site: %d", st)
	}
	admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "other.example.com", "kind": "static"}, &other)

	type appView struct {
		ID             string  `json:"id"`
		Kind           string  `json:"kind"`
		HostPort       *int    `json:"host_port"`
		ContainerPort  *int    `json:"container_port"`
		SiteID         *string `json:"site_id"`
		SiteDomain     *string `json:"site_domain"`
		SiteUser       *string `json:"site_user"`
		Directory      *string `json:"directory"`
		Command        *string `json:"command"`
		BuildCommand   *string `json:"build_command"`
		Runtime        string  `json:"runtime"`
		RuntimeVersion *string `json:"runtime_version"`
		BuildState     string  `json:"build_state"`
		BuildLog       *string `json:"build_log"`
		State          string  `json:"state"`
	}
	base := func() map[string]any {
		return map[string]any{"server_id": serverID, "name": "admin-front", "kind": "service", "site_id": site.ID,
			"directory": "/frontend/", "command": "pnpm start admin", "build_command": "pnpm install && pnpm build",
			"runtime": "node", "runtime_version": "24", "domain": "admin.example.com"}
	}
	var e errBody
	for _, bad := range []struct {
		change func(map[string]any)
		code   string
	}{
		{func(m map[string]any) { delete(m, "site_id") }, "site_required"},
		{func(m map[string]any) { m["site_id"] = "8a6a3c8e-1111-4222-8333-944444444444" }, "site_not_found"},
		{func(m map[string]any) { m["directory"] = "../etc" }, "invalid_directory"},
		{func(m map[string]any) { m["command"] = "" }, "invalid_command"},
		{func(m map[string]any) { m["command"] = "node a.js\nnode b.js" }, "invalid_command"},
		{func(m map[string]any) { m["runtime_version"] = "18" }, "invalid_runtime"},
		{func(m map[string]any) { m["runtime"] = "bun" }, "invalid_runtime"},
		{func(m map[string]any) { m["container_port"] = 80 }, "invalid_port"},
	} {
		body := base()
		bad.change(body)
		if st := admin.do("POST", "/api/v1/apps", body, &e); e.Code != bad.code {
			t.Errorf("%v: %d %s, want %s", body, st, e.Code, bad.code)
		}
	}

	// A Next.js app from a monorepo folder, with a domain: the panel picks its port.
	var front appView
	if st := admin.do("POST", "/api/v1/apps", base(), &front); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if front.Kind != "service" || front.HostPort == nil || *front.HostPort < 20000 || front.ContainerPort == nil || *front.ContainerPort != *front.HostPort ||
		*front.Directory != "frontend" || *front.SiteDomain != "license.example.com" || *front.SiteUser != site.LinuxUser {
		t.Fatalf("created: %+v", front)
	}
	eventually(t, "the app reaches the agent", func() bool { return spec("admin-front").ID == front.ID })
	got := spec("admin-front")
	if got.Kind != "service" || got.User != site.LinuxUser || got.Directory != "frontend" || got.Command != "pnpm start admin" ||
		got.Runtime != "node" || got.RuntimeVersion != "24" || got.HostPort != *front.HostPort || got.Domain != "admin.example.com" {
		t.Fatalf("spec %+v", got)
	}

	// A second app from the same folder, on a port the program insists on.
	body := base()
	body["name"], body["domain"], body["command"], body["container_port"] = "portal-front", "portal.example.com", "pnpm start portal", 4009
	var portal appView
	if st := admin.do("POST", "/api/v1/apps", body, &portal); st != http.StatusCreated || *portal.HostPort != 4009 || *portal.ContainerPort != 4009 {
		t.Fatalf("fixed port: %d %+v", st, portal)
	}
	body["name"], body["domain"] = "portal-two", "portal2.example.com"
	if st := admin.do("POST", "/api/v1/apps", body, &e); st != http.StatusConflict || e.Code != "port_taken" {
		t.Errorf("the same port twice: %d %s", st, e.Code)
	}
	body["container_port"] = *front.HostPort
	if st := admin.do("POST", "/api/v1/apps", body, &e); st != http.StatusConflict || e.Code != "port_taken" {
		t.Errorf("another app's chosen port: %d %s", st, e.Code)
	}

	// A worker: no domain, no port, no runtime.
	var worker appView
	if st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "queue", "kind": "service", "site_id": site.ID,
		"command": "php8.4 artisan horizon"}, &worker); st != http.StatusCreated || worker.HostPort != nil || worker.Runtime != "" || *worker.Directory != "" {
		t.Fatalf("worker: %d %+v", st, worker)
	}

	// Changing the folder or command, and changing a fixed port to one the panel picks.
	body = base()
	body["command"] = "pnpm start admin --turbo"
	var updated appView
	if st := admin.do("PUT", "/api/v1/apps/"+front.ID, body, &updated); st != http.StatusOK || *updated.Command != "pnpm start admin --turbo" || *updated.HostPort != *front.HostPort {
		t.Fatalf("update: %d %+v", st, updated)
	}
	body = base()
	body["name"], body["domain"], body["command"] = "portal-front", "portal.example.com", "pnpm start portal"
	if st := admin.do("PUT", "/api/v1/apps/"+portal.ID, body, &updated); st != http.StatusOK || *updated.HostPort < 20000 {
		t.Fatalf("back to a chosen port: %d %+v", st, updated)
	}

	// Building runs the build command as the app, then restarts it.
	var task struct {
		ID string `json:"id"`
	}
	if st := admin.do("POST", "/api/v1/apps/"+front.ID+"/build", nil, &task); st != http.StatusAccepted || task.ID == "" {
		t.Fatalf("build: %d", st)
	}
	eventually(t, "the build finishes", func() bool {
		var a appView
		admin.do("GET", "/api/v1/apps/"+front.ID, nil, &a)
		return a.BuildState == "ok" && a.BuildLog != nil && strings.Contains(*a.BuildLog, "pnpm build")
	})
	mu.Lock()
	if len(builds) != 1 || builds[0].Command != "pnpm install && pnpm build" || builds[0].App.User != site.LinuxUser || restarted["admin-front"] != 1 {
		t.Errorf("build %+v, restarts %v", builds, restarted)
	}
	buildOK = false
	mu.Unlock()
	admin.do("POST", "/api/v1/apps/"+front.ID+"/build", nil, nil)
	eventually(t, "the failed build is recorded", func() bool {
		var a appView
		admin.do("GET", "/api/v1/apps/"+front.ID, nil, &a)
		return a.BuildState == "failed"
	})
	mu.Lock()
	if restarted["admin-front"] != 1 {
		t.Error("a failed build must not restart the app")
	}
	mu.Unlock()
	if st := admin.do("POST", "/api/v1/apps/"+worker.ID+"/build", nil, &e); st != http.StatusBadRequest || e.Code != "no_build_command" {
		t.Errorf("build without a command: %d %s", st, e.Code)
	}
	var list []appView
	admin.do("GET", "/api/v1/apps", nil, &list)
	for _, a := range list {
		if a.BuildLog != nil {
			t.Error("the app list must leave out build output")
		}
	}

	// What a service app does not have.
	for _, c := range []struct {
		method, path string
		body         any
		code         string
	}{
		{"POST", "/api/v1/apps/" + front.ID + "/update", nil, "service_app_update"},
		{"POST", "/api/v1/cron", map[string]any{"app_id": front.ID, "schedule": "* * * * *", "command": "ls"}, "service_app_cron"},
		{"DELETE", "/api/v1/sites/" + site.ID, nil, "site_has_service_apps"},
	} {
		if st := admin.do(c.method, c.path, c.body, &e); e.Code != c.code {
			t.Errorf("%s %s: %d %s, want %s", c.method, c.path, st, e.Code, c.code)
		}
	}

	// Customers: they cannot set one up, and their service apps follow their website.
	var pkg idOnly
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Node", "max_sites": 2, "max_apps": 3, "server_ids": []string{serverID}}, &pkg)
	var created struct {
		Customer idOnly `json:"customer"`
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &created)
	cust := login(t, p.web.URL, "c@example.com", created.Password)
	var custSite siteView
	if st := cust.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "c.example.com", "kind": "static"}, &custSite); st != http.StatusCreated {
		t.Fatalf("customer site: %d", st)
	}
	if st := cust.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "mine", "kind": "service", "site_id": custSite.ID,
		"command": "node server.js"}, &e); st != http.StatusForbidden || e.Code != "service_not_allowed" {
		t.Errorf("customer service app: %d %s", st, e.Code)
	}
	// An administrator sets one up for them, from their website only.
	if st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "theirs", "kind": "service", "site_id": site.ID,
		"command": "node server.js", "owner_id": created.Customer.ID}, &e); st != http.StatusBadRequest || e.Code != "site_other_owner" {
		t.Errorf("someone else's website: %d %s", st, e.Code)
	}
	var theirs appView
	if st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "theirs", "kind": "service", "site_id": custSite.ID,
		"command": "node server.js", "listens": true, "owner_id": created.Customer.ID}, &theirs); st != http.StatusCreated || theirs.HostPort == nil {
		t.Fatalf("for a customer: %d %+v", st, theirs)
	}
	var seen appView
	if st := cust.do("GET", "/api/v1/apps/"+theirs.ID, nil, &seen); st != http.StatusOK {
		t.Fatalf("the customer sees it: %d", st)
	}
	if st := cust.do("PUT", "/api/v1/apps/"+theirs.ID, map[string]any{"site_id": custSite.ID, "command": "node evil.js"}, &e); st != http.StatusForbidden || e.Code != "service_not_allowed" {
		t.Errorf("customer changes the command: %d %s", st, e.Code)
	}
	if st := cust.do("POST", "/api/v1/apps/"+theirs.ID+"/restart", nil, nil); st >= 400 && st != http.StatusBadGateway && st != http.StatusConflict {
		t.Errorf("customer restart: %d", st)
	}
	if st := admin.do("POST", "/api/v1/owner-changes", map[string]any{"to": "", "apps": []string{theirs.ID}}, &e); st != http.StatusConflict || e.Code != "service_app_owner" {
		t.Errorf("the app alone to another owner: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/owner-changes", map[string]any{"to": "", "sites": []string{custSite.ID}}, nil); st != http.StatusOK {
		t.Fatalf("the website to the administrators: %d", st)
	}
	var moved struct {
		OwnerID *string `json:"owner_id"`
	}
	admin.do("GET", "/api/v1/apps/"+theirs.ID, nil, &moved)
	if moved.OwnerID != nil {
		t.Errorf("the service app must follow its website: %v", *moved.OwnerID)
	}

	// Deleting the apps frees the website.
	for _, id := range []string{front.ID, portal.ID, worker.ID} {
		admin.do("DELETE", "/api/v1/apps/"+id, nil, nil)
	}
	eventually(t, "the apps are gone", func() bool {
		var l []appView
		admin.do("GET", "/api/v1/apps?server_id="+serverID, nil, &l)
		for _, a := range l {
			if a.SiteID != nil && *a.SiteID == site.ID {
				return false
			}
		}
		return true
	})
	time.Sleep(10 * time.Millisecond)
	if st := admin.do("DELETE", "/api/v1/sites/"+site.ID, nil, &e); st >= 300 {
		t.Errorf("delete the website afterwards: %d %s", st, e.Code)
	}
}
