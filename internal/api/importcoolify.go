package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// Importing applications and services from Coolify (administrators): the panel reads them
// through Coolify's API with a token the administrator gives for this one request, and makes
// ekPanel apps of them. Settings come over; the data in volumes does not.

// coolifyClient reads Coolify's API (v4, /api/v1).
type coolifyClient struct {
	base, token string
	http        *http.Client
}

var errCoolifyAuth = errors.New("coolify refused the token")

func newCoolifyClient(raw, token string) (*coolifyClient, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, errors.New("invalid address")
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 512 || strings.ContainsAny(token, " \r\n") {
		return nil, errors.New("invalid token")
	}
	base := strings.TrimSuffix(u.Scheme+"://"+u.Host+strings.TrimSuffix(u.Path, "/"), "/api/v1")
	return &coolifyClient{base: base, token: token, http: &http.Client{Timeout: 30 * time.Second,
		// The token goes to the address given, nowhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func (c *coolifyClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errCoolifyAuth
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	return json.Unmarshal(body, out)
}

func (s *Server) writeCoolifyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCoolifyAuth):
		writeError(w, http.StatusBadRequest, "coolify_unauthorized", "Coolify did not accept the token")
	default:
		writeErrorData(w, http.StatusBadGateway, "coolify_unreachable", "could not read Coolify's API: "+err.Error(), map[string]any{"detail": err.Error()})
	}
}

// coolifyResource is one application, service or database in Coolify, as the import page
// shows it, with what it becomes here.
type coolifyResource struct {
	UUID    string   `json:"uuid"`
	Kind    string   `json:"kind"` // application, service, database
	Name    string   `json:"name"`
	Type    string   `json:"type"` // build pack, service type or database type
	Status  string   `json:"status,omitempty"`
	Domains []string `json:"domains"`
	Image   string   `json:"image,omitempty"`
	GitURL  string   `json:"git_url,omitempty"`
	// AppName is the name of the app it becomes here; AppKind its kind (image, compose,
	// git), or "" when it cannot be imported (Reason says why).
	AppName string `json:"app_name"`
	AppKind string `json:"app_kind"`
	Reason  string `json:"reason,omitempty"`
}

type anyMap = map[string]any

func str(m anyMap, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// hostsOf reads Coolify's fqdn lists ("https://a.example.com,http://b.example.com:3000").
func hostsOf(list string) (hosts []string, port int) {
	for _, f := range strings.Split(list, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "://") {
			f = "https://" + f
		}
		u, err := url.Parse(f)
		if err != nil || u.Hostname() == "" {
			continue
		}
		if p, err := strconv.Atoi(u.Port()); err == nil && port == 0 {
			port = p
		}
		if h := strings.ToLower(u.Hostname()); !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	return hosts, port
}

var (
	appNameCleanRE = regexp.MustCompile(`[^a-z0-9]+`)
	coolifyUUIDRE  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// appNameFor makes an ekPanel app name (2–32 lowercase letters, digits and dashes,
// starting with a letter) of a Coolify name.
func appNameFor(name string) string {
	n := strings.Trim(appNameCleanRE.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if n == "" || n[0] < 'a' || n[0] > 'z' {
		n = "app-" + n
	}
	if len(n) > 32 {
		n = strings.TrimRight(n[:32], "-")
	}
	if len(n) < 2 {
		n += "-app"
	}
	return n
}

// coolifyGitURL makes Coolify's repository (owner/repo for its GitHub apps, or an address)
// one ekPanel can clone.
func coolifyGitURL(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}
	return "https://github.com/" + strings.TrimSuffix(repo, ".git")
}

func applicationResource(a anyMap) coolifyResource {
	r := coolifyResource{UUID: str(a, "uuid"), Kind: "application", Name: str(a, "name"), Type: str(a, "build_pack"), Status: str(a, "status")}
	r.Domains, _ = hostsOf(str(a, "fqdn"))
	r.AppName = appNameFor(r.Name)
	switch r.Type {
	case "dockerimage":
		r.AppKind = "image"
		r.Image = str(a, "docker_registry_image_name")
		if tag := str(a, "docker_registry_image_tag"); tag != "" && !strings.Contains(r.Image, "@") {
			r.Image += ":" + tag
		}
	case "dockercompose":
		r.AppKind = "compose"
		r.Domains = composeDomains(str(a, "docker_compose_domains"))
	case "dockerfile", "nixpacks", "railpack", "static":
		r.AppKind = "git"
		r.GitURL = coolifyGitURL(str(a, "git_repository"))
		if r.GitURL == "" {
			r.AppKind, r.Reason = "", "no_repository"
		}
	default:
		r.Reason = "build_pack"
	}
	return r
}

// composeDomains reads docker_compose_domains: {"service": {"domain": "https://…"}}.
func composeDomains(raw string) []string {
	var m map[string]struct {
		Domain string `json:"domain"`
	}
	json.Unmarshal([]byte(raw), &m)
	var out []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}) {
		h, _ := hostsOf(m[k].Domain)
		out = append(out, h...)
	}
	return out
}

func (s *Server) listCoolify(ctx context.Context, c *coolifyClient) ([]coolifyResource, error) {
	out := []coolifyResource{}
	var apps, services, dbs []anyMap
	if err := c.get(ctx, "/applications", &apps); err != nil {
		return nil, err
	}
	if err := c.get(ctx, "/services", &services); err != nil {
		return nil, err
	}
	if err := c.get(ctx, "/databases", &dbs); err != nil {
		return nil, err
	}
	for _, a := range apps {
		out = append(out, applicationResource(a))
	}
	for _, sv := range services {
		r := coolifyResource{UUID: str(sv, "uuid"), Kind: "service", Name: str(sv, "name"), Type: str(sv, "service_type"), Status: str(sv, "status"),
			AppKind: "compose"}
		r.AppName = appNameFor(r.Name)
		if subs, ok := sv["applications"].([]any); ok {
			for _, x := range subs {
				if m, ok := x.(anyMap); ok {
					h, _ := hostsOf(str(m, "fqdn"))
					r.Domains = append(r.Domains, h...)
				}
			}
		}
		if str(sv, "docker_compose_raw") == "" {
			r.AppKind, r.Reason = "", "no_compose"
		}
		out = append(out, r)
	}
	for _, d := range dbs {
		r := coolifyResource{UUID: str(d, "uuid"), Kind: "database", Name: str(d, "name"), Type: str(d, "database_type"), Status: str(d, "status"),
			Image: str(d, "image"), Reason: "database", AppName: appNameFor(str(d, "name"))}
		out = append(out, r)
	}
	for i := range out {
		if out[i].Domains == nil {
			out[i].Domains = []string{}
		}
	}
	return out, nil
}

type coolifyRequest struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func (s *Server) handleInspectCoolify(w http.ResponseWriter, r *http.Request) {
	var req coolifyRequest
	if !readJSON(w, r, &req) {
		return
	}
	c, err := newCoolifyClient(req.URL, req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_coolify", "enter Coolify's address (https://…) and an API token")
		return
	}
	list, err := s.listCoolify(r.Context(), c)
	if err != nil {
		s.writeCoolifyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"resources": list})
}

type coolifyImportRequest struct {
	coolifyRequest
	ServerID  string `json:"server_id"`
	OwnerID   string `json:"owner_id"`
	Resources []struct {
		Kind string `json:"kind"`
		UUID string `json:"uuid"`
		Name string `json:"name"` // the app's name here
	} `json:"resources"`
}

// coolifyResult is what became of one resource.
type coolifyResult struct {
	UUID    string             `json:"uuid"`
	Name    string             `json:"name"`
	AppID   string             `json:"app_id,omitempty"`
	Code    string             `json:"code,omitempty"` // an error code, as the API's
	Message string             `json:"message,omitempty"`
	Data    map[string]any     `json:"data,omitempty"`
	Notes   []tasks.ImportNote `json:"notes"`
}

// captured is a ResponseWriter that keeps an error answer, so the checks of app creation
// can run for each imported app.
type captured struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (c *captured) Header() http.Header {
	if c.h == nil {
		c.h = http.Header{}
	}
	return c.h
}
func (c *captured) Write(b []byte) (int, error) { return c.body.Write(b) }
func (c *captured) WriteHeader(code int)        { c.status = code }

func (c *captured) failed(res *coolifyResult) bool {
	if c.status == 0 || c.status < 300 {
		return false
	}
	var e map[string]any
	json.Unmarshal(c.body.Bytes(), &e)
	res.Code, _ = e["code"].(string)
	res.Message, _ = e["message"].(string)
	delete(e, "code")
	delete(e, "message")
	if len(e) > 0 {
		res.Data = e
	}
	return true
}

func (s *Server) handleImportCoolify(w http.ResponseWriter, r *http.Request) {
	var req coolifyImportRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	c, err := newCoolifyClient(req.URL, req.Token)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_coolify", "enter Coolify's address (https://…) and an API token")
		return
	}
	if len(req.Resources) == 0 {
		writeError(w, http.StatusBadRequest, "nothing_to_import", "choose at least one application or service")
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	srv, err := s.store.GetServer(ctx, req.ServerID)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	place, ok := s.resolveOwner(w, r, req.OwnerID)
	if !ok {
		return
	}
	// Check Coolify answers before making anything.
	var probe []anyMap
	if err := c.get(ctx, "/applications", &probe); err != nil {
		s.writeCoolifyError(w, err)
		return
	}
	u := currentUser(r)
	results := []coolifyResult{}
	made := 0
	for _, want := range req.Resources {
		res := coolifyResult{UUID: want.UUID, Name: want.Name, Notes: []tasks.ImportNote{}}
		app, notes, err := s.coolifyApp(ctx, c, want.Kind, want.UUID)
		if err != nil {
			res.Code, res.Message = "coolify_resource", err.Error()
			results = append(results, res)
			continue
		}
		res.Notes = append(res.Notes, notes...)
		app.Name = strings.TrimSpace(want.Name)
		cw := &captured{}
		created, ok := s.createImportedApp(cw, r, app, place, srv.ID, u.ID)
		if !ok {
			cw.failed(&res)
			results = append(results, res)
			continue
		}
		res.AppID = created.ID
		made++
		s.audit(r, "import.coolify", created.Name, map[string]any{"app_id": created.ID, "coolify_uuid": want.UUID, "kind": want.Kind})
		results = append(results, res)
	}
	if made > 0 {
		s.queueReconcile(r, srv.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// createImportedApp runs the checks and steps of app creation for one imported app; on a
// failure the error answer is in w.
func (s *Server) createImportedApp(w http.ResponseWriter, r *http.Request, req appRequest, place placement, serverID, by string) (store.App, bool) {
	if !appNameRE.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_app_name", "use 2–32 lowercase letters, digits and dashes, starting with a letter")
		return store.App{}, false
	}
	set, ok := appSettings(w, req.Kind, req)
	if !ok || !customerAppRules(w, place.Package, req.Kind, set, nil) || !s.allowNew(w, r, place, "app", serverID) {
		return store.App{}, false
	}
	n := store.NewApp{ServerID: serverID, Name: req.Name, Kind: req.Kind, OwnerID: place.OwnerID, CreatedBy: &by, AppSettings: set}
	if req.Kind == "git" {
		if err := s.newGitSecrets(&n); err != nil {
			s.internalError(w, r, err)
			return store.App{}, false
		}
	}
	app, err := s.store.CreateApp(r.Context(), n)
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return store.App{}, false
	}
	return app, true
}

// coolifyApp reads one Coolify application or service and makes the app request for it,
// with notes on what does not come over.
func (s *Server) coolifyApp(ctx context.Context, c *coolifyClient, kind, uuid string) (appRequest, []tasks.ImportNote, error) {
	var req appRequest
	var notes []tasks.ImportNote
	if !coolifyUUIDRE.MatchString(uuid) {
		return req, nil, errors.New("invalid resource")
	}
	base := map[string]string{"application": "/applications/", "service": "/services/"}[kind]
	if base == "" {
		return req, nil, errors.New("only applications and services can be imported")
	}
	var obj anyMap
	if err := c.get(ctx, base+uuid, &obj); err != nil {
		return req, nil, err
	}
	// Variables: values come with a token that may read sensitive data.
	var envs []anyMap
	if err := c.get(ctx, base+uuid+"/envs", &envs); err != nil {
		return req, nil, err
	}
	hidden := 0
	for _, e := range envs {
		if e["is_preview"] == true {
			continue
		}
		key, value := str(e, "key"), str(e, "value")
		if v := str(e, "real_value"); v != "" {
			value = v
		}
		if key == "" || slices.ContainsFunc(req.Env, func(x store.EnvVar) bool { return x.Key == key }) {
			continue
		}
		if _, ok := e["value"]; !ok {
			hidden++
		}
		req.Env = append(req.Env, store.EnvVar{Key: key, Value: value})
	}
	if hidden > 0 {
		notes = append(notes, tasks.ImportNote{Code: "coolify_env_hidden", Count: hidden})
	}
	var storages struct {
		Persistent []anyMap `json:"persistent_storages"`
		Files      []anyMap `json:"file_storages"`
	}
	c.get(ctx, base+uuid+"/storages", &storages) // older Coolify has no such endpoint
	for _, st := range storages.Persistent {
		if p := str(st, "mount_path"); p != "" {
			req.Volumes = append(req.Volumes, p)
		}
	}
	if len(storages.Persistent) > 0 {
		notes = append(notes, tasks.ImportNote{Code: "coolify_volumes", Count: len(storages.Persistent)})
	}
	if len(storages.Files) > 0 {
		notes = append(notes, tasks.ImportNote{Code: "coolify_files", Count: len(storages.Files)})
	}

	if kind == "service" {
		req.Kind = "compose"
		req.Compose = declareComposeVolumes(str(obj, "docker_compose_raw"))
		// The service's parts with a domain: the first one is routed here.
		var subs []anyMap
		if list, ok := obj["applications"].([]any); ok {
			for _, x := range list {
				if m, ok := x.(anyMap); ok {
					subs = append(subs, m)
				}
			}
		} else {
			c.get(ctx, base+uuid+"/applications", &subs)
		}
		for _, sub := range subs {
			hosts, port := hostsOf(str(sub, "fqdn"))
			if len(hosts) == 0 {
				continue
			}
			if req.Domain != "" {
				notes = append(notes, tasks.ImportNote{Code: "coolify_more_domains", Name: strings.Join(hosts, ", ")})
				continue
			}
			req.Service, req.Domain = str(sub, "name"), hosts[0]
			if port == 0 {
				port = servicePortFromEnv(req.Env, req.Service)
			}
			if port != 0 {
				req.ContainerPort = &port
			}
			if len(hosts) > 1 {
				notes = append(notes, tasks.ImportNote{Code: "coolify_more_domains", Name: strings.Join(hosts[1:], ", ")})
			}
		}
		if req.Domain != "" && req.ContainerPort == nil {
			notes = append(notes, tasks.ImportNote{Code: "coolify_no_port", Name: req.Domain})
			req.Domain, req.Service = "", ""
		}
		return req, notes, nil
	}

	r := applicationResource(obj)
	req.Kind = r.AppKind
	hosts, port := hostsOf(str(obj, "fqdn"))
	if port == 0 {
		if p, err := strconv.Atoi(strings.TrimSpace(strings.Split(str(obj, "ports_exposes"), ",")[0])); err == nil {
			port = p
		}
	}
	if port != 0 {
		req.ContainerPort = &port
	}
	switch r.AppKind {
	case "image":
		req.Image = r.Image
	case "git":
		notes = append(notes, tasks.ImportNote{Code: "coolify_deploy"}) // nothing runs before the first build
		req.GitURL, req.GitBranch = r.GitURL, str(obj, "git_branch")
		if req.GitBranch == "" {
			req.GitBranch = "main"
		}
		base := strings.Trim(str(obj, "base_directory"), "/")
		if base == "" {
			base = "."
		}
		req.BuildContext = base
		switch r.Type {
		case "dockerfile":
			req.Builder = "dockerfile"
			req.Dockerfile = strings.TrimPrefix(str(obj, "dockerfile_location"), "/")
			if base != "." && req.Dockerfile != "" {
				req.Dockerfile = base + "/" + req.Dockerfile
			}
		default:
			req.Builder = "railpack" // Coolify's Nixpacks and static builds
			notes = append(notes, tasks.ImportNote{Code: "coolify_builder", Name: r.Type})
		}
		if strings.HasPrefix(req.GitURL, "git@") || str(obj, "private_key_id") != "" && str(obj, "private_key_id") != "0" {
			notes = append(notes, tasks.ImportNote{Code: "coolify_deploy_key"})
		}
	case "compose":
		req.Compose = declareComposeVolumes(str(obj, "docker_compose_raw"))
		var m map[string]struct {
			Domain string `json:"domain"`
		}
		json.Unmarshal([]byte(str(obj, "docker_compose_domains")), &m)
		for _, svc := range slices.Sorted(func(yield func(string) bool) {
			for k := range m {
				if !yield(k) {
					return
				}
			}
		}) {
			h, p := hostsOf(m[svc].Domain)
			if len(h) == 0 {
				continue
			}
			if req.Domain == "" && p != 0 {
				req.Service, req.Domain, req.ContainerPort = svc, h[0], &p
				continue
			}
			notes = append(notes, tasks.ImportNote{Code: "coolify_more_domains", Name: strings.Join(h, ", ")})
		}
		hosts = nil
	default:
		return req, nil, fmt.Errorf("Coolify's %q builds cannot be imported", r.Type)
	}
	if len(hosts) > 0 {
		if req.ContainerPort == nil {
			notes = append(notes, tasks.ImportNote{Code: "coolify_no_port", Name: hosts[0]})
		} else {
			req.Domain = hosts[0]
		}
		if len(hosts) > 1 {
			notes = append(notes, tasks.ImportNote{Code: "coolify_more_domains", Name: strings.Join(hosts[1:], ", ")})
		}
	}
	return req, notes, nil
}

// servicePortFromEnv finds the port of a service's part in Coolify's SERVICE_FQDN_<PART>_<PORT>
// (or SERVICE_URL_…) variables.
func servicePortFromEnv(env []store.EnvVar, part string) int {
	norm := func(s string) string { return strings.ToUpper(appNameCleanRE.ReplaceAllString(strings.ToLower(s), "")) }
	want := norm(part)
	for _, e := range env {
		for _, prefix := range []string{"SERVICE_FQDN_", "SERVICE_URL_"} {
			rest, ok := strings.CutPrefix(e.Key, prefix)
			i := strings.LastIndexByte(rest, '_')
			if !ok || i < 0 || norm(rest[:i]) != want {
				continue
			}
			if p, err := strconv.Atoi(rest[i+1:]); err == nil && p > 0 && p < 65536 {
				return p
			}
		}
	}
	return 0
}

var (
	// shortVolumeRE is a short volume entry: - name:/path (quoted or not, maybe :ro).
	shortVolumeRE = regexp.MustCompile(`^\s*-\s*['"]?([A-Za-z0-9][A-Za-z0-9_.-]*):/`)
	// sourceVolumeRE is a long one's source: name.
	sourceVolumeRE = regexp.MustCompile(`^\s*source:\s*['"]?([A-Za-z0-9][A-Za-z0-9_.-]*)['"]?\s*$`)
	topVolumesRE   = regexp.MustCompile(`(?m)^volumes:\s*$`)
	topKeyRE       = regexp.MustCompile(`(?m)^\S`)
)

// declareComposeVolumes adds the named volumes services use to the file's top-level
// volumes: Coolify's raw Compose files leave them out (Coolify adds them as it deploys),
// and Compose refuses a volume that is not declared.
func declareComposeVolumes(compose string) string {
	var names []string
	for _, line := range strings.Split(compose, "\n") {
		m := shortVolumeRE.FindStringSubmatch(line)
		if m == nil {
			m = sourceVolumeRE.FindStringSubmatch(line)
		}
		if m != nil && !slices.Contains(names, m[1]) {
			names = append(names, m[1])
		}
	}
	var missing []string
	top := topVolumesRE.FindStringIndex(compose)
	for _, n := range names {
		declared := false
		if top != nil {
			// A key of the top-level volumes: an indented "name:" after it, before the next top-level key.
			rest := compose[top[1]:]
			if end := topKeyRE.FindStringIndex(rest); end != nil {
				rest = rest[:end[0]]
			}
			declared = regexp.MustCompile(`(?m)^\s+['"]?` + regexp.QuoteMeta(n) + `['"]?:`).MatchString(rest)
		}
		if !declared {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return compose
	}
	var b strings.Builder
	for _, n := range missing {
		b.WriteString("\n  " + n + ": {}")
	}
	if top != nil {
		return compose[:top[1]] + b.String() + compose[top[1]:]
	}
	return strings.TrimRight(compose, "\n") + "\nvolumes:" + b.String() + "\n"
}
