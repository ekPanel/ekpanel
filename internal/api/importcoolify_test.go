package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakeCoolify answers Coolify's API with a few resources.
func fakeCoolify(t *testing.T) *httptest.Server {
	apps := []map[string]any{
		{"uuid": "a1", "name": "Ghost Blog", "build_pack": "dockerimage", "docker_registry_image_name": "ghost", "docker_registry_image_tag": "5",
			"fqdn": "https://blog.example.com,https://www.blog.example.com", "ports_exposes": "2368", "status": "running:healthy"},
		{"uuid": "a2", "name": "API", "build_pack": "nixpacks", "git_repository": "acme/api", "git_branch": "prod", "ports_exposes": "3000",
			"fqdn": "https://api.example.com", "base_directory": "/"},
		{"uuid": "a3", "name": "stack", "build_pack": "dockercompose", "docker_compose_raw": "services:\n  web:\n    image: nginx\n",
			"docker_compose_domains": `{"web":{"domain":"https://stack.example.com:80"}}`},
	}
	services := []map[string]any{{"uuid": "s1", "name": "uptime kuma", "service_type": "uptime-kuma",
		"docker_compose_raw": "services:\n  uptime-kuma:\n    image: louislam/uptime-kuma:1\n    environment:\n      - SERVICE_FQDN_UPTIMEKUMA_3001\n",
		"applications":       []map[string]any{{"name": "uptime-kuma", "fqdn": "https://status.example.com"}}}}
	envs := map[string]any{
		"a1": []map[string]any{{"key": "url", "value": "https://blog.example.com", "is_preview": false},
			{"key": "database__client", "value": "sqlite3", "is_preview": false}, {"key": "url", "value": "preview", "is_preview": true}},
		"a2": []map[string]any{{"key": "SECRET", "is_preview": false}},
		"a3": []map[string]any{},
		"s1": []map[string]any{{"key": "SERVICE_FQDN_UPTIMEKUMA_3001", "value": "https://status.example.com", "is_preview": false}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		var out any
		switch {
		case p == "/applications":
			out = apps
		case p == "/services":
			out = services
		case p == "/databases":
			out = []map[string]any{{"uuid": "d1", "name": "pg", "database_type": "standalone-postgresql", "image": "postgres:16"}}
		case strings.HasSuffix(p, "/envs"):
			out = envs[strings.Split(p, "/")[2]]
		case strings.HasSuffix(p, "/storages"):
			if strings.Contains(p, "a1") {
				out = map[string]any{"persistent_storages": []map[string]any{{"name": "content", "mount_path": "/var/lib/ghost/content"}}, "file_storages": []any{}}
			} else {
				out = map[string]any{"persistent_storages": []any{}, "file_storages": []any{}}
			}
		case strings.HasPrefix(p, "/applications/"):
			for _, a := range apps {
				if a["uuid"] == strings.TrimPrefix(p, "/applications/") {
					out = a
				}
			}
		case p == "/services/s1":
			out = services[0]
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestImportCoolify(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	srvID, _, _, _ := enrolled(t, p, c, "web")
	cool := fakeCoolify(t)

	var e errBody
	if st := c.do("POST", "/api/v1/imports/coolify/inspect", map[string]any{"url": cool.URL, "token": "bad"}, &e); st != http.StatusBadRequest || e.Code != "coolify_unauthorized" {
		t.Fatalf("bad token: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/imports/coolify/inspect", map[string]any{"url": "ftp://x", "token": "t"}, &e); st != http.StatusBadRequest || e.Code != "invalid_coolify" {
		t.Fatalf("bad address: %d %s", st, e.Code)
	}
	var list struct {
		Resources []struct {
			UUID    string   `json:"uuid"`
			Kind    string   `json:"kind"`
			AppName string   `json:"app_name"`
			AppKind string   `json:"app_kind"`
			Domains []string `json:"domains"`
			Reason  string   `json:"reason"`
		} `json:"resources"`
	}
	if st := c.do("POST", "/api/v1/imports/coolify/inspect", map[string]any{"url": cool.URL + "/", "token": "good-token"}, &list); st != http.StatusOK {
		t.Fatalf("inspect: %d", st)
	}
	var got []string
	for _, r := range list.Resources {
		got = append(got, r.UUID+" "+r.Kind+" "+r.AppName+" "+r.AppKind+" "+strings.Join(r.Domains, ",")+" "+r.Reason)
	}
	want := []string{"a1 application ghost-blog image blog.example.com,www.blog.example.com ", "a2 application api git api.example.com ",
		"a3 application stack compose stack.example.com ", "s1 service uptime-kuma compose status.example.com ", "d1 database pg   database"}
	if !slices.Equal(got, want) {
		t.Fatalf("resources:\n%q\nwant\n%q", got, want)
	}

	var res struct {
		Results []struct {
			UUID  string `json:"uuid"`
			AppID string `json:"app_id"`
			Code  string `json:"code"`
			Notes []struct {
				Code  string `json:"code"`
				Name  string `json:"name"`
				Count int    `json:"count"`
			} `json:"notes"`
		} `json:"results"`
	}
	body := map[string]any{"url": cool.URL, "token": "good-token", "server_id": srvID, "resources": []map[string]string{
		{"kind": "application", "uuid": "a1", "name": "ghost-blog"}, {"kind": "application", "uuid": "a2", "name": "api"},
		{"kind": "application", "uuid": "a3", "name": "stack"}, {"kind": "service", "uuid": "s1", "name": "uptime-kuma"},
		{"kind": "application", "uuid": "a1", "name": "Bad Name"}}}
	if st := c.do("POST", "/api/v1/imports/coolify", body, &res); st != http.StatusOK || len(res.Results) != 5 {
		t.Fatalf("import: %d %+v", st, res)
	}
	for _, r := range res.Results[:4] {
		if r.AppID == "" || r.Code != "" {
			t.Fatalf("result %+v", r)
		}
	}
	if res.Results[4].Code != "invalid_app_name" {
		t.Fatalf("a bad name: %+v", res.Results[4])
	}
	notes := map[string][]string{}
	for _, r := range res.Results {
		for _, n := range r.Notes {
			notes[r.UUID] = append(notes[r.UUID], n.Code+" "+n.Name)
		}
	}
	if !slices.Contains(notes["a1"], "coolify_volumes ") || !slices.Contains(notes["a1"], "coolify_more_domains www.blog.example.com") ||
		!slices.Contains(notes["a2"], "coolify_builder nixpacks") || !slices.Contains(notes["a2"], "coolify_env_hidden ") {
		t.Fatalf("notes %v", notes)
	}

	type app struct {
		Kind          string            `json:"kind"`
		Image         *string           `json:"image"`
		Domain        *string           `json:"domain"`
		ContainerPort *int              `json:"container_port"`
		Service       *string           `json:"service"`
		Volumes       []string          `json:"volumes"`
		Env           []map[string]any  `json:"env"`
		GitURL        *string           `json:"git_url"`
		GitBranch     *string           `json:"git_branch"`
		Builder       string            `json:"builder"`
		Extra         map[string]string `json:"-"`
	}
	get := func(id string) app {
		var a app
		c.do("GET", "/api/v1/apps/"+id, nil, &a)
		return a
	}
	ghost := get(res.Results[0].AppID)
	if ghost.Kind != "image" || *ghost.Image != "ghost:5" || *ghost.Domain != "blog.example.com" || *ghost.ContainerPort != 2368 ||
		!slices.Equal(ghost.Volumes, []string{"/var/lib/ghost/content"}) || len(ghost.Env) != 2 {
		t.Fatalf("ghost %+v", ghost)
	}
	api := get(res.Results[1].AppID)
	if api.Kind != "git" || *api.GitURL != "https://github.com/acme/api" || *api.GitBranch != "prod" || api.Builder != "railpack" || *api.ContainerPort != 3000 {
		t.Fatalf("api %+v", api)
	}
	stack := get(res.Results[2].AppID)
	if stack.Kind != "compose" || *stack.Service != "web" || *stack.ContainerPort != 80 || *stack.Domain != "stack.example.com" {
		t.Fatalf("stack %+v", stack)
	}
	kuma := get(res.Results[3].AppID)
	if kuma.Kind != "compose" || *kuma.Service != "uptime-kuma" || *kuma.ContainerPort != 3001 || *kuma.Domain != "status.example.com" || len(kuma.Env) != 1 {
		t.Fatalf("kuma %+v", kuma)
	}
}
