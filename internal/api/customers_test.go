package api_test

import (
	"context"
	"net/http"
	"testing"

	"ekpanel/internal/db"
)

func login(t *testing.T, base, email, password string) *client {
	t.Helper()
	c := newClient(t, base)
	if st := c.do("POST", "/api/v1/auth/login", map[string]string{"email": email, "password": password}, nil); st != http.StatusOK {
		t.Fatalf("login %s: %d", email, st)
	}
	return c
}

func TestCustomersAndPackages(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")
	otherServer, _, _, _ := enrolled(t, p, admin, "web-2")

	var pkg struct {
		ID string `json:"id"`
	}
	if st := admin.do("POST", "/api/v1/packages", map[string]any{"name": "Starter", "max_sites": 1, "max_databases": 1, "max_apps": 1,
		"disk_mb": 1, "php_versions": []string{"8.4"}, "app_images": []string{"nginx"}, "server_ids": []string{serverID}, "sftp": false}, &pkg); st != http.StatusCreated {
		t.Fatalf("create package: %d", st)
	}
	var e errBody
	if st := admin.do("POST", "/api/v1/packages", map[string]any{"name": "starter"}, &e); st != http.StatusConflict || e.Code != "package_name_taken" {
		t.Errorf("same name: %d %s", st, e.Code)
	}
	type created struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Password string `json:"password"`
	}
	var ca, cb created
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "a@example.com", "name": "Customer A", "package_id": pkg.ID}, &ca)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "b@example.com", "name": "Customer B", "package_id": pkg.ID}, &cb)
	if ca.Password == "" || cb.Customer.ID == "" {
		t.Fatalf("customers not created: %+v %+v", ca, cb)
	}
	if st := admin.do("POST", "/api/v1/customers", map[string]string{"email": "A@example.com", "name": "Again", "package_id": pkg.ID}, &e); st != http.StatusConflict || e.Code != "email_taken" {
		t.Errorf("same email: %d %s", st, e.Code)
	}
	a := login(t, p.web.URL, "a@example.com", ca.Password)
	b := login(t, p.web.URL, "b@example.com", cb.Password)

	// Package limits.
	type idOnly struct {
		ID string `json:"id"`
	}
	for _, c := range []struct {
		path string
		body map[string]any
		code string
	}{
		{"/api/v1/sites", map[string]any{"server_id": otherServer, "domain": "a.example.com", "kind": "static"}, "server_not_allowed"},
		{"/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "php", "php_version": "8.3"}, "php_version_not_allowed"},
		{"/api/v1/apps", map[string]any{"server_id": serverID, "name": "stack", "kind": "compose", "compose": "services: {}"}, "compose_not_allowed"},
		{"/api/v1/apps", map[string]any{"server_id": serverID, "name": "cache", "kind": "image", "image": "redis:7"}, "image_not_allowed"},
		{"/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "kind": "image", "image": "nginxevil:1"}, "image_not_allowed"},
	} {
		if st := a.do("POST", c.path, c.body, &e); e.Code != c.code {
			t.Errorf("%s %v: %d %s, want %s", c.path, c.body, st, e.Code, c.code)
		}
	}
	var site, app idOnly
	var dbc struct {
		Database idOnly `json:"database"`
	}
	if st := a.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "php", "php_version": "8.4"}, &site); st != http.StatusCreated {
		t.Fatalf("site: %d", st)
	}
	if st := a.do("POST", "/api/v1/databases", map[string]any{"server_id": serverID, "engine": "mariadb", "name": "shop"}, &dbc); st != http.StatusCreated {
		t.Fatalf("database: %d", st)
	}
	if st := a.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "kind": "image", "image": "nginx:alpine"}, &app); st != http.StatusCreated {
		t.Fatalf("app: %d", st)
	}
	for _, c := range []struct {
		path string
		body map[string]any
		code string
	}{
		{"/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a2.example.com", "kind": "static"}, "limit_reached_site"},
		{"/api/v1/databases", map[string]any{"server_id": serverID, "engine": "mariadb", "name": "shop2"}, "limit_reached_database"},
		{"/api/v1/apps", map[string]any{"server_id": serverID, "name": "web2", "kind": "image", "image": "nginx"}, "limit_reached_app"},
	} {
		if st := a.do("POST", c.path, c.body, &e); st != http.StatusConflict || e.Code != c.code {
			t.Errorf("second on %s: %d %s", c.path, st, e.Code)
		}
	}
	if st := a.do("POST", "/api/v1/sites/"+site.ID+"/sftp", map[string]bool{"enabled": true}, &e); st != http.StatusForbidden || e.Code != "sftp_not_allowed" {
		t.Errorf("sftp: %d %s", st, e.Code)
	}

	// Another customer cannot see or touch any of it, on any route.
	s, d, ap := "/api/v1/sites/"+site.ID, "/api/v1/databases/"+dbc.Database.ID, "/api/v1/apps/"+app.ID
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", s, nil}, {"PATCH", s, map[string]any{"kind": "static"}}, {"DELETE", s, nil},
		{"POST", s + "/sftp", map[string]bool{"enabled": false}}, {"POST", s + "/sftp/password", nil},
		{"PUT", s + "/ssh-keys", map[string]any{"keys": []string{}}}, {"POST", s + "/wordpress", map[string]string{"admin_user": "owner"}},
		{"POST", s + "/files", map[string]string{"action": "list", "path": "/"}},
		{"POST", s + "/files/upload?path=/x&offset=0", nil}, {"GET", s + "/files/download?path=/x", nil},
		{"GET", d, nil}, {"POST", d + "/password", nil}, {"DELETE", d, nil},
		{"GET", ap, nil}, {"PUT", ap, map[string]any{"image": "nginx"}}, {"DELETE", ap, nil}, {"POST", ap + "/start", nil},
		{"POST", ap + "/stop", nil}, {"POST", ap + "/restart", nil}, {"POST", ap + "/update", nil},
		{"GET", ap + "/status", nil}, {"GET", ap + "/logs", nil},
	} {
		if st := b.do(c.method, c.path, c.body, nil); st != http.StatusNotFound {
			t.Errorf("customer B %s %s: %d, want 404", c.method, c.path, st)
		}
	}
	for _, path := range []string{"/api/v1/sites", "/api/v1/databases", "/api/v1/apps", "/api/v1/tasks"} {
		var list []idOnly
		b.do("GET", path, nil, &list)
		if len(list) != 0 {
			t.Errorf("customer B sees %d items at %s", len(list), path)
		}
		a.do("GET", path, nil, &list)
		if path != "/api/v1/tasks" && len(list) != 1 {
			t.Errorf("customer A sees %d items at %s", len(list), path)
		}
	}
	// Administration stays with administrators.
	for _, path := range []string{"/api/v1/packages", "/api/v1/customers", "/api/v1/backups", "/api/v1/audit", "/api/v1/servers/" + serverID,
		"/api/v1/servers/" + serverID + "/firewall"} {
		if st := a.do("GET", path, nil, nil); st != http.StatusForbidden {
			t.Errorf("customer GET %s: %d", path, st)
		}
	}
	var servers []map[string]any
	a.do("GET", "/api/v1/servers", nil, &servers)
	if len(servers) != 1 || servers[0]["id"] != serverID || servers[0]["hostname"] != nil {
		t.Errorf("customer servers %v", servers)
	}
	var versions struct {
		Versions []string `json:"versions"`
		Default  string   `json:"default"`
	}
	a.do("GET", "/api/v1/php-versions", nil, &versions)
	if len(versions.Versions) != 1 || versions.Default != "8.4" {
		t.Errorf("php versions %+v", versions)
	}

	// Disk quota: once used up, files cannot grow.
	pool, err := db.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	pool.Exec(context.Background(), `UPDATE sites SET disk_bytes = 2 * 1024 * 1024 WHERE id = $1`, site.ID)
	if st := a.do("POST", s+"/files", map[string]string{"action": "write", "path": "/public_html/x.txt"}, &e); st != http.StatusConflict || e.Code != "quota_exceeded" {
		t.Errorf("write over quota: %d %s", st, e.Code)
	}
	var usage struct {
		Usage struct {
			Sites     int   `json:"sites"`
			DiskBytes int64 `json:"disk_bytes"`
		} `json:"usage"`
		Package struct {
			Name string `json:"name"`
		} `json:"package"`
	}
	a.do("GET", "/api/v1/me/usage", nil, &usage)
	if usage.Package.Name != "Starter" || usage.Usage.Sites != 1 || usage.Usage.DiskBytes != 2<<20 {
		t.Errorf("usage %+v", usage)
	}

	// Signing in as a customer.
	if st := admin.do("POST", "/api/v1/customers/"+cb.Customer.ID+"/impersonate", nil, nil); st != http.StatusOK {
		t.Fatalf("impersonate: %d", st)
	}
	var me struct {
		ID             string  `json:"id"`
		ImpersonatorID *string `json:"impersonator_id"`
	}
	admin.do("GET", "/api/v1/auth/me", nil, &me)
	if me.ID != cb.Customer.ID || me.ImpersonatorID == nil {
		t.Fatalf("me while impersonating %+v", me)
	}
	if st := admin.do("GET", "/api/v1/packages", nil, nil); st != http.StatusForbidden {
		t.Errorf("admin routes while impersonating: %d", st)
	}
	if st := admin.do("POST", "/api/v1/auth/2fa/setup", nil, nil); st != http.StatusForbidden {
		t.Errorf("2FA setup while impersonating: %d", st)
	}
	admin.do("POST", "/api/v1/databases", map[string]any{"server_id": serverID, "engine": "mariadb", "name": "bshop"}, nil)
	me.ImpersonatorID = nil
	if st := admin.do("POST", "/api/v1/auth/impersonation/end", nil, &me); st != http.StatusOK || me.ImpersonatorID != nil || me.ID == cb.Customer.ID {
		t.Fatalf("end: %d %+v", st, me)
	}
	if st := admin.do("GET", "/api/v1/packages", nil, nil); st != http.StatusOK {
		t.Errorf("back as admin: %d", st)
	}
	var audit []struct {
		Action  string         `json:"action"`
		ActorID *string        `json:"actor_id"`
		Details map[string]any `json:"details"`
	}
	admin.do("GET", "/api/v1/audit", nil, &audit)
	found := false
	for _, x := range audit {
		if x.Action == "database.created" && x.Details["as_customer"] == cb.Customer.ID {
			found = x.ActorID != nil && *x.ActorID != cb.Customer.ID
		}
	}
	if !found {
		t.Error("an action while impersonating must name the administrator and the customer")
	}

	// A customer who owns things cannot be deleted; the package of customers neither.
	if st := admin.do("DELETE", "/api/v1/customers/"+ca.Customer.ID, nil, &e); st != http.StatusConflict || e.Code != "customer_has_resources" {
		t.Errorf("delete customer: %d %s", st, e.Code)
	}
	if st := admin.do("DELETE", "/api/v1/packages/"+pkg.ID, nil, &e); st != http.StatusConflict || e.Code != "package_in_use" {
		t.Errorf("delete package: %d %s", st, e.Code)
	}

	// Own password.
	if st := a.do("POST", "/api/v1/auth/password", map[string]string{"current": "wrong", "new": "a-new-long-password"}, &e); e.Code != "invalid_password" {
		t.Errorf("wrong current password: %d %s", st, e.Code)
	}
	if st := a.do("POST", "/api/v1/auth/password", map[string]string{"current": ca.Password, "new": "a-new-long-password"}, nil); st != http.StatusNoContent {
		t.Errorf("change password: %d", st)
	}
	if st := a.do("GET", "/api/v1/sites", nil, nil); st != http.StatusOK {
		t.Errorf("session after password change: %d", st)
	}
	login(t, p.web.URL, "a@example.com", "a-new-long-password")
}
