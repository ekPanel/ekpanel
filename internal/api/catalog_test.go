package api_test

import (
	"net/http"
	"strings"
	"testing"
)

type catalogApp struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	Compose         string `json:"compose"`
	Service         string `json:"service"`
	ContainerPort   int    `json:"container_port"`
	Domain          string `json:"domain"`
	Template        string `json:"template"`
	TemplateVersion int    `json:"template_version"`
	PublicPorts     bool   `json:"public_ports"`
	Env             []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"env"`
}

func (a catalogApp) env(key string) string {
	for _, e := range a.Env {
		if e.Key == key {
			return e.Value
		}
	}
	return ""
}

func TestCatalog(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, _, _, _ := enrolled(t, p, admin, "web-1")

	var list []struct {
		ID      string   `json:"id"`
		Images  []string `json:"images"`
		Compose string   `json:"compose"`
		Allowed bool     `json:"allowed"`
	}
	if st := admin.do("GET", "/api/v1/catalog", nil, &list); st != http.StatusOK || len(list) < 10 || !list[0].Allowed || list[0].Compose == "" {
		t.Fatalf("catalog: %d %+v", st, list)
	}

	// Installing: the template decides the Compose file, port and variables.
	var e errBody
	if st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "cloud", "template": "nextcloud"}, &e); st != http.StatusBadRequest || e.Code != "domain_required" {
		t.Errorf("no domain: %d %s", st, e.Code)
	}
	if st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "xx", "template": "nope"}, &e); e.Code != "template_not_found" {
		t.Errorf("unknown template: %d %s", st, e.Code)
	}
	var app catalogApp
	st := admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "cloud", "template": "nextcloud", "domain": "cloud.example.com",
		"compose": "services: {evil: {image: x, privileged: true}}", "public_ports": true, "env": []map[string]string{{"key": "ADMIN_PASSWORD", "value": "mine"}}}, &app)
	if st != http.StatusCreated {
		t.Fatalf("install: %d", st)
	}
	pw := app.env("ADMIN_PASSWORD")
	if app.Kind != "compose" || app.Template != "nextcloud" || app.TemplateVersion < 1 || app.Service != "nextcloud" || app.ContainerPort != 80 ||
		strings.Contains(app.Compose, "evil") || app.PublicPorts || len(pw) != 24 || app.env("DOMAIN") != "cloud.example.com" {
		t.Fatalf("installed app: %+v", app)
	}

	// Updating: the Compose file stays the template's, values made from the domain follow it.
	var up catalogApp
	if st := admin.do("PUT", "/api/v1/apps/"+app.ID, map[string]any{"compose": "services: {evil: {image: x}}", "service": "evil", "container_port": 9,
		"domain": "files.example.com", "env": app.Env}, &up); st != http.StatusOK {
		t.Fatalf("update: %d", st)
	}
	if up.Compose != app.Compose || up.Service != "nextcloud" || up.ContainerPort != 80 || up.env("DOMAIN") != "files.example.com" || up.env("ADMIN_PASSWORD") != pw {
		t.Fatalf("updated app: %+v", up)
	}

	// Customers may install templates, within their package's images.
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Apps", "max_apps": 2, "app_images": []string{"louislam/uptime-kuma", "redis"}}, &pkg)
	var c struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &c)
	cust := login(t, p.web.URL, "c@example.com", c.Password)
	allowed := map[string]bool{}
	cust.do("GET", "/api/v1/catalog", nil, &list)
	for _, l := range list {
		allowed[l.ID] = l.Allowed
	}
	if !allowed["uptime-kuma"] || !allowed["redis"] || allowed["nextcloud"] {
		t.Errorf("allowed for the customer: %v", allowed)
	}
	if st := cust.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "cloud2", "template": "nextcloud", "domain": "c.example.com"}, &e); st != http.StatusForbidden || e.Code != "image_not_allowed" {
		t.Errorf("template outside the package: %d %s", st, e.Code)
	}
	var kuma catalogApp
	if st := cust.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "status", "template": "uptime-kuma", "domain": "status.example.com"}, &kuma); st != http.StatusCreated {
		t.Fatalf("customer install: %d", st)
	}
	if st := cust.do("PUT", "/api/v1/apps/"+kuma.ID, map[string]any{"compose": "services: {x: {image: alpine, privileged: true}}", "domain": "s.example.com"}, &up); st != http.StatusOK ||
		up.Compose != kuma.Compose || up.Domain != "s.example.com" {
		t.Errorf("customer update: %d %+v", st, up)
	}
	// Their own Compose files are still refused.
	if st := cust.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "own", "kind": "compose", "compose": "services: {}"}, &e); e.Code != "compose_not_allowed" {
		t.Errorf("own compose: %d %s", st, e.Code)
	}
}
