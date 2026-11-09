package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

type tokenBody struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Prefix string   `json:"prefix"`
	Scopes []string `json:"scopes"`
	Token  string   `json:"token"`
}

func TestAPITokens(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")
	var e errBody

	// Making tokens: names, scopes and expiry are checked.
	for _, bad := range []map[string]any{
		{"name": "", "scopes": []string{"read"}},
		{"name": "ci", "scopes": []string{}},
		{"name": "ci", "scopes": []string{"everything"}},
		{"name": "ci", "scopes": []string{"read"}, "expires_in_days": 7},
	} {
		if st := admin.do("POST", "/api/v1/tokens", bad, &e); st != http.StatusBadRequest {
			t.Errorf("%v: %d %s", bad, st, e.Code)
		}
	}
	var ro, rw tokenBody
	if st := admin.do("POST", "/api/v1/tokens", map[string]any{"name": "read only", "scopes": []string{"read"}, "expires_in_days": 30}, &ro); st != http.StatusCreated || !strings.HasPrefix(ro.Token, "ekp_") || !strings.HasPrefix(ro.Token, ro.Prefix) || len(ro.Token) < 40 {
		t.Fatalf("create: %d %+v", st, ro)
	}
	admin.do("POST", "/api/v1/tokens", map[string]any{"name": "deploy", "scopes": []string{"sites", "read", "sites"}}, &rw)
	if strings.Join(rw.Scopes, ",") != "read,sites" {
		t.Fatalf("scopes: %v", rw.Scopes)
	}
	var list []tokenBody
	if admin.do("GET", "/api/v1/tokens", nil, &list); len(list) != 2 || list[0].Token != "" {
		t.Fatalf("list: %+v (the token itself is never listed)", list)
	}

	call := func(tok, method, path string, body, out any) int {
		return newClient(t, p.web.URL).do(method, path, body, out, "Authorization", "Bearer "+tok)
	}

	// Reading works with "read"; changes need their scope.
	var servers []map[string]any
	if st := call(ro.Token, "GET", "/api/v1/servers", nil, &servers); st != http.StatusOK || len(servers) != 1 {
		t.Fatalf("GET servers with read: %d %v", st, servers)
	}
	var me map[string]any
	if st := call(ro.Token, "GET", "/api/v1/auth/me", nil, &me); st != http.StatusOK || me["email"] != "admin@example.com" {
		t.Fatalf("me: %d %v", st, me)
	}
	site := map[string]any{"domain": "tok.example.com", "server_id": serverID, "kind": "static"}
	if st := call(ro.Token, "POST", "/api/v1/sites", site, &e); st != http.StatusForbidden || e.Code != "token_scope" {
		t.Fatalf("create site with read: %d %s", st, e.Code)
	}
	if st := call(rw.Token, "POST", "/api/v1/sites", site, nil); st != http.StatusCreated {
		t.Fatalf("create site with sites: %d", st)
	}
	if st := call(rw.Token, "POST", "/api/v1/servers", map[string]string{"name": "x"}, &e); st != http.StatusForbidden || e.Code != "token_scope" {
		t.Fatalf("add a server with sites: %d %s", st, e.Code)
	}

	// Public reads still work when a token is sent.
	for _, path := range []string{"/api/v1/health", "/api/v1/openapi.json", "/api/v1/setup/status"} {
		if st := call(ro.Token, "GET", path, nil, nil); st != http.StatusOK {
			t.Errorf("GET %s with a token: %d", path, st)
		}
	}

	// Any token may see whose it is and read tasks, even without "read".
	var only tokenBody
	admin.do("POST", "/api/v1/tokens", map[string]any{"name": "backups only", "scopes": []string{"backups"}}, &only)
	if st := call(only.Token, "GET", "/api/v1/auth/me", nil, &me); st != http.StatusOK {
		t.Fatalf("me with backups only: %d", st)
	}
	var tasks []struct {
		ID string `json:"id"`
	}
	if st := call(only.Token, "GET", "/api/v1/tasks", nil, &tasks); st != http.StatusOK || len(tasks) == 0 {
		t.Fatalf("tasks with backups only: %d %v", st, tasks)
	}
	var task map[string]any
	if st := call(only.Token, "GET", "/api/v1/tasks/"+tasks[0].ID, nil, &task); st != http.StatusOK || task["id"] != tasks[0].ID {
		t.Fatalf("one task: %d %v", st, task)
	}
	if st := call(only.Token, "GET", "/api/v1/tasks/00000000-0000-0000-0000-000000000000", nil, &e); st != http.StatusNotFound {
		t.Fatalf("unknown task: %d", st)
	}
	if st := call(only.Token, "GET", "/api/v1/sites", nil, &e); st != http.StatusForbidden || e.Code != "token_scope" {
		t.Fatalf("sites with backups only: %d %s", st, e.Code)
	}

	// Never with a token: tokens, account security, terminals.
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/tokens"},
		{"POST", "/api/v1/tokens"},
		{"POST", "/api/v1/auth/password"},
		{"POST", "/api/v1/auth/logout"},
		{"GET", "/api/v1/servers/" + serverID + "/terminal"},
		{"POST", "/api/v1/customers/00000000-0000-0000-0000-000000000000/impersonate"},
	} {
		if st := call(rw.Token, c.method, c.path, map[string]any{}, &e); st != http.StatusForbidden || e.Code != "token_not_allowed" {
			t.Errorf("%s %s: %d %s", c.method, c.path, st, e.Code)
		}
	}
	// A customers-scoped token with a made-up cookie cannot sign in as a customer.
	var cust tokenBody
	admin.do("POST", "/api/v1/tokens", map[string]any{"name": "customers", "scopes": []string{"customers"}}, &cust)
	if st := call(cust.Token, "POST", "/api/v1/customers/00000000-0000-0000-0000-000000000000/impersonate", map[string]any{}, &e); st != http.StatusForbidden || e.Code != "token_not_allowed" {
		t.Fatalf("impersonate with a token: %d %s", st, e.Code)
	}
	// Webhooks are not read as token requests, whatever Authorization header they bring.
	if st := call("anything", "POST", "/api/v1/hooks/git/00000000-0000-0000-0000-000000000000", map[string]any{}, &e); st == http.StatusUnauthorized && e.Code == "invalid_token" {
		t.Fatalf("webhook with an Authorization header: %d %s", st, e.Code)
	}

	// Wrong and malformed tokens.
	if st := call("ekp_nope", "GET", "/api/v1/servers", nil, &e); st != http.StatusUnauthorized || e.Code != "invalid_token" {
		t.Fatalf("unknown token: %d %s", st, e.Code)
	}
	if st := admin.do("GET", "/api/v1/servers", nil, &e, "Authorization", "Basic abc"); st != http.StatusUnauthorized {
		t.Fatalf("basic auth (even with a session cookie): %d", st)
	}

	// The audit log names the token.
	var audit []struct {
		Action  string         `json:"action"`
		Details map[string]any `json:"details"`
	}
	admin.do("GET", "/api/v1/audit", nil, &audit)
	found := false
	for _, a := range audit {
		if a.Action == "site.created" && a.Details["via_token"] == "deploy" {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit has no site.created via the deploy token: %+v", audit)
	}

	// Last use is recorded; revoked and expired tokens stop working.
	var used []map[string]any
	admin.do("GET", "/api/v1/tokens", nil, &used)
	if used[0]["last_used_at"] == nil {
		t.Fatalf("last use not recorded: %v", used[0])
	}
	if st := admin.do("DELETE", "/api/v1/tokens/"+rw.ID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("revoke: %d", st)
	}
	if st := call(rw.Token, "GET", "/api/v1/servers", nil, &e); st != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d", st)
	}
	p.pool.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 second' WHERE id = $1`, ro.ID)
	if st := call(ro.Token, "GET", "/api/v1/servers", nil, &e); st != http.StatusUnauthorized {
		t.Fatalf("expired token: %d", st)
	}
}

func TestCustomerTokens(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Starter", "max_sites": 2, "max_databases": 1, "max_apps": 1,
		"disk_mb": 100, "php_versions": []string{"8.4"}, "app_images": []string{"nginx"}, "server_ids": []string{serverID}}, &pkg)
	var created struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &created)
	c := login(t, p.web.URL, "c@example.com", created.Password)

	var e errBody
	if st := c.do("POST", "/api/v1/tokens", map[string]any{"name": "x", "scopes": []string{"servers"}}, &e); st != http.StatusBadRequest || e.Code != "invalid_scope" {
		t.Fatalf("customer asks for servers: %d %s", st, e.Code)
	}
	var tok tokenBody
	if st := c.do("POST", "/api/v1/tokens", map[string]any{"name": "x", "scopes": []string{"read", "sites"}}, &tok); st != http.StatusCreated {
		t.Fatalf("customer token: %d", st)
	}
	// The token is still the customer: no administrator pages.
	if st := newClient(t, p.web.URL).do("GET", "/api/v1/customers", nil, &e, "Authorization", "Bearer "+tok.Token); st != http.StatusForbidden {
		t.Fatalf("customer token on customers: %d", st)
	}
	var sites []any
	if st := newClient(t, p.web.URL).do("GET", "/api/v1/sites", nil, &sites, "Authorization", "Bearer "+tok.Token); st != http.StatusOK {
		t.Fatalf("customer token on sites: %d", st)
	}
	// An administrator signed in as the customer cannot make tokens for them.
	var cust []struct {
		ID string `json:"id"`
	}
	admin.do("GET", "/api/v1/customers", nil, &cust)
	admin.do("POST", "/api/v1/customers/"+cust[0].ID+"/impersonate", nil, nil)
	if st := admin.do("POST", "/api/v1/tokens", map[string]any{"name": "y", "scopes": []string{"read"}}, &e); st != http.StatusForbidden {
		t.Fatalf("token while impersonating: %d %s", st, e.Code)
	}
}
