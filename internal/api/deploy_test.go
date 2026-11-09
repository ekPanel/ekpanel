package api_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agentproto"
)

// deployAgent builds by streaming a few lines and reporting a commit, and applies state by
// reporting every app as applied, except a release of failImage.
type deployAgent struct {
	mu        sync.Mutex
	builds    []agentproto.BuildParams
	applied   []agentproto.AppSpec
	failBuild bool
	failImage string
}

func (fa *deployAgent) run(conn *websocket.Conn) {
	write := func(env agentproto.Envelope) {
		b, _ := json.Marshal(env)
		conn.Write(context.Background(), websocket.MessageText, b)
	}
	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var env agentproto.Envelope
			json.Unmarshal(data, &env)
			if env.Type != agentproto.TypeRequest {
				continue
			}
			resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: env.ID, Result: json.RawMessage("{}")}
			switch env.Op {
			case agentproto.OpAppBuild:
				var p agentproto.BuildParams
				json.Unmarshal(env.Params, &p)
				fa.mu.Lock()
				fa.builds = append(fa.builds, p)
				fail := fa.failBuild
				fa.mu.Unlock()
				write(resp)
				write(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: p.Stream, Data: []byte("==> Building ")})
				write(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: p.Stream, Data: []byte("Bangla বাংলা\n")})
				end := agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: p.Stream, Reason: "exit"}
				if fail {
					end.Reason = "the build failed: exit status 1"
				} else {
					end.Result, _ = json.Marshal(agentproto.BuildResult{Commit: "c0ffee" + strings.Repeat("0", 34), Message: "Add the shop"})
				}
				write(end)
				continue
			case agentproto.OpStateApply:
				var p agentproto.StateApplyParams
				json.Unmarshal(env.Params, &p)
				res := agentproto.StateApplyResult{}
				fa.mu.Lock()
				for _, ap := range p.Apps {
					fa.applied = append(fa.applied, ap)
					r := agentproto.AppResult{ID: ap.ID, Generation: ap.Generation, OK: true}
					if fa.failImage != "" && ap.Image == fa.failImage {
						r.OK, r.Error = false, "the new release did not start, so the previous one keeps running"
					}
					res.Apps = append(res.Apps, r)
				}
				fa.mu.Unlock()
				resp.Result, _ = json.Marshal(res)
			}
			write(resp)
		}
	}()
}

type deployment struct {
	ID               string `json:"id"`
	Number           int    `json:"number"`
	Status           string `json:"status"`
	Trigger          string `json:"trigger"`
	CommitSHA        string `json:"commit_sha"`
	CommitMessage    string `json:"commit_message"`
	Image            string `json:"image"`
	Error            string `json:"error"`
	RollbackPossible bool   `json:"rollback_possible"`
}

// waitDeployment polls a deployment until it is done.
func waitDeployment(t *testing.T, c *client, appID, id string) deployment {
	t.Helper()
	var d deployment
	for range 100 {
		c.do("GET", "/api/v1/apps/"+appID+"/deployments/"+id, nil, &d)
		if d.Status == "live" || d.Status == "failed" || d.Status == "cancelled" {
			return d
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("deployment still %s", d.Status)
	return d
}

func signed(t *testing.T, url, secret, event string, body string, headers ...string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(body))
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	req.Header.Set("X-GitHub-Event", event)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Del("X-GitHub-Event")
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestGitDeploys(t *testing.T) {
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
	fa := &deployAgent{}
	fa.run(conn)

	// Settings are checked.
	var e errBody
	base := map[string]any{"server_id": serverID, "name": "shop", "kind": "git", "git_url": "https://github.com/acme/shop.git",
		"git_branch": "main", "container_port": 3000, "domain": "shop.example.com", "env": []map[string]string{{"key": "TOKEN", "value": "s3cret"}}}
	for field, bad := range map[string]any{"git_url": "file:///etc/passwd", "git_branch": "../x", "dockerfile": "../../etc/shadow", "container_port": nil} {
		body := map[string]any{}
		for k, v := range base {
			body[k] = v
		}
		body[field] = bad
		if st := admin.do("POST", "/api/v1/apps", body, &e); st != http.StatusBadRequest {
			t.Errorf("%s=%v: %d %s", field, bad, st, e.Code)
		}
	}
	var app struct {
		ID              string `json:"id"`
		Kind            string `json:"kind"`
		Image           *string
		HostPort        int    `json:"host_port"`
		Dockerfile      string `json:"dockerfile"`
		BuildContext    string `json:"build_context"`
		AutoDeploy      bool   `json:"auto_deploy"`
		DeployKeyPublic string `json:"deploy_key_public"`
	}
	if st := admin.do("POST", "/api/v1/apps", base, &app); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if app.Kind != "git" || app.Image != nil || app.Dockerfile != "Dockerfile" || app.BuildContext != "." || !app.AutoDeploy ||
		!strings.HasPrefix(app.DeployKeyPublic, "ssh-ed25519 ") || !strings.HasSuffix(app.DeployKeyPublic, " ekpanel-shop") {
		t.Fatalf("git app: %+v", app)
	}
	// Two host ports: the next app starts after both.
	var other struct {
		HostPort int `json:"host_port"`
	}
	admin.do("POST", "/api/v1/apps", map[string]any{"server_id": serverID, "name": "web", "kind": "image", "image": "nginx", "container_port": 80}, &other)
	if other.HostPort != app.HostPort+2 {
		t.Errorf("ports: git app %d, next app %d", app.HostPort, other.HostPort)
	}
	var info struct {
		WebhookURL    string `json:"webhook_url"`
		WebhookSecret string `json:"webhook_secret"`
	}
	admin.do("GET", "/api/v1/apps/"+app.ID+"/git", nil, &info)
	if !strings.HasSuffix(info.WebhookURL, "/api/v1/hooks/git/"+app.ID) || len(info.WebhookSecret) != 64 {
		t.Fatalf("git info: %+v", info)
	}

	// A deploy builds, then releases the build.
	var d deployment
	if st := admin.do("POST", "/api/v1/apps/"+app.ID+"/deploy", nil, &d); st != http.StatusAccepted || d.Number != 1 {
		t.Fatalf("deploy: %d %+v", st, d)
	}
	d1 := waitDeployment(t, admin, app.ID, d.ID)
	if d1.Status != "live" || d1.CommitMessage != "Add the shop" || !strings.HasPrefix(d1.Image, "ekpanel/shop:") {
		t.Fatalf("first deploy: %+v", d1)
	}
	fa.mu.Lock()
	b := fa.builds[0]
	fa.mu.Unlock()
	if b.Repo != "https://github.com/acme/shop.git" || b.Branch != "main" || b.Image != d1.Image || !strings.Contains(b.SSHKey, "OPENSSH PRIVATE KEY") ||
		len(b.Env) != 1 || b.Env[0].Value != "s3cret" {
		t.Fatalf("build params: %+v", b)
	}
	var log struct {
		Log    string `json:"log"`
		Length int    `json:"length"`
	}
	admin.do("GET", "/api/v1/apps/"+app.ID+"/deployments/"+d.ID+"/log", nil, &log)
	if !strings.Contains(log.Log, "==> Building Bangla বাংলা") || !strings.Contains(log.Log, "==> Live") {
		t.Fatalf("log:\n%s", log.Log)
	}
	var rest struct {
		Log string `json:"log"`
	}
	admin.do("GET", "/api/v1/apps/"+app.ID+"/deployments/"+d.ID+"/log?offset=10", nil, &rest)
	if rest.Log != string([]rune(log.Log)[10:]) {
		t.Errorf("log from an offset: %q", rest.Log)
	}
	var cur struct {
		Image string `json:"image"`
		State string `json:"state"`
	}
	admin.do("GET", "/api/v1/apps/"+app.ID, nil, &cur)
	if cur.Image != d1.Image {
		t.Fatalf("the app runs %q", cur.Image)
	}
	if st := admin.do("POST", "/api/v1/apps/"+app.ID+"/update", nil, &e); e.Code != "git_app_update" {
		t.Errorf("image update of a git app: %d %s", st, e.Code)
	}

	// The webhook: signed pushes to the branch deploy that commit; anything else does not.
	hook := p.web.URL + "/api/v1/hooks/git/" + app.ID
	push := `{"ref":"refs/heads/main","after":"` + strings.Repeat("ab", 20) + `"}`
	if st := signed(t, hook, "wrong", "push", push); st != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d", st)
	}
	if st := signed(t, hook, "", "push", push); st != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", st)
	}
	if st := signed(t, hook, info.WebhookSecret, "ping", `{}`); st != http.StatusOK {
		t.Errorf("ping: %d", st)
	}
	if st := signed(t, hook, info.WebhookSecret, "push", `{"ref":"refs/heads/dev","after":"`+strings.Repeat("ab", 20)+`"}`); st != http.StatusAccepted {
		t.Errorf("other branch: %d", st)
	}
	if st := signed(t, hook, info.WebhookSecret, "push", push); st != http.StatusAccepted {
		t.Fatalf("push: %d", st)
	}
	var list []deployment
	admin.do("GET", "/api/v1/apps/"+app.ID+"/deployments", nil, &list)
	if len(list) != 2 || list[0].Trigger != "webhook" {
		t.Fatalf("deployments after the hook: %+v", list)
	}
	d2 := waitDeployment(t, admin, app.ID, list[0].ID)
	fa.mu.Lock()
	if d2.Status != "live" || fa.builds[1].Commit != strings.Repeat("ab", 20) {
		t.Fatalf("webhook deploy: %+v %+v", d2, fa.builds[1])
	}
	fa.mu.Unlock()
	// GitLab sends the secret itself.
	if st := signed(t, hook, "", "Push Hook", push, "X-Gitlab-Event", "Push Hook", "X-Gitlab-Token", info.WebhookSecret); st != http.StatusAccepted {
		t.Errorf("gitlab: %d", st)
	}
	admin.do("GET", "/api/v1/apps/"+app.ID+"/deployments", nil, &list)
	waitDeployment(t, admin, app.ID, list[0].ID)

	// Rolling back returns to an earlier build without building.
	admin.do("GET", "/api/v1/apps/"+app.ID+"/deployments/"+d1.ID, nil, &d1)
	if d1.Status != "superseded" || !d1.RollbackPossible {
		t.Fatalf("earlier deploy: %+v", d1)
	}
	fa.mu.Lock()
	builds := len(fa.builds)
	fa.mu.Unlock()
	var rb deployment
	if st := admin.do("POST", "/api/v1/apps/"+app.ID+"/deployments/"+d1.ID+"/rollback", nil, &rb); st != http.StatusAccepted || rb.Trigger != "rollback" {
		t.Fatalf("rollback: %d %+v", st, rb)
	}
	rb = waitDeployment(t, admin, app.ID, rb.ID)
	admin.do("GET", "/api/v1/apps/"+app.ID, nil, &cur)
	fa.mu.Lock()
	if rb.Status != "live" || cur.Image != d1.Image || len(fa.builds) != builds || rb.CommitMessage != "Add the shop" {
		t.Fatalf("rolled back: %+v, app runs %s, builds %d", rb, cur.Image, len(fa.builds))
	}
	fa.mu.Unlock()
	if st := admin.do("POST", "/api/v1/apps/"+app.ID+"/deployments/"+rb.ID+"/rollback", nil, &e); e.Code != "rollback_not_possible" {
		t.Errorf("rollback to the live one: %d %s", st, e.Code)
	}

	// A failed build changes nothing; a release that does not start goes back to the build
	// that still runs.
	fa.mu.Lock()
	fa.failBuild = true
	fa.mu.Unlock()
	admin.do("POST", "/api/v1/apps/"+app.ID+"/deploy", nil, &d)
	if d = waitDeployment(t, admin, app.ID, d.ID); d.Status != "failed" || !strings.Contains(d.Error, "exit status 1") {
		t.Fatalf("failed build: %+v", d)
	}
	fa.mu.Lock()
	fa.failBuild = false
	fa.mu.Unlock()
	admin.do("POST", "/api/v1/apps/"+app.ID+"/deploy", nil, &d)
	fa.mu.Lock()
	fa.failImage = "ekpanel/shop:" + strings.ReplaceAll(d.ID, "-", "")[:12]
	fa.mu.Unlock()
	if d = waitDeployment(t, admin, app.ID, d.ID); d.Status != "failed" || !strings.Contains(d.Error, "previous one keeps running") {
		t.Fatalf("failed release: %+v", d)
	}
	admin.do("GET", "/api/v1/apps/"+app.ID, nil, &cur)
	if cur.Image != d1.Image {
		t.Fatalf("after a failed release the app must name the build that runs, %s, not %s", d1.Image, cur.Image)
	}

	// Customers cannot have Git apps (yet).
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "P", "max_apps": 3}, &pkg)
	var c struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &c)
	cust := login(t, p.web.URL, "c@example.com", c.Password)
	body := map[string]any{}
	for k, v := range base {
		body[k] = v
	}
	body["name"] = "mine"
	if st := cust.do("POST", "/api/v1/apps", body, &e); st != http.StatusForbidden || e.Code != "git_not_allowed" {
		t.Errorf("customer git app: %d %s", st, e.Code)
	}
	if st := cust.do("POST", "/api/v1/apps/"+app.ID+"/deploy", nil, &e); st != http.StatusNotFound {
		t.Errorf("customer deploying the admin's app: %d", st)
	}
	if st := signed(t, p.web.URL+"/api/v1/hooks/git/00000000-0000-4000-8000-000000000001", info.WebhookSecret, "push", push); st != http.StatusNotFound {
		t.Errorf("hook of an unknown app: %d", st)
	}
}

func prEvent(action string, number int, head, base string, fork bool) string {
	headRepo := 1
	if fork {
		headRepo = 2
	}
	return fmt.Sprintf(`{"action":%q,"number":%d,"pull_request":{"number":%d,"head":{"ref":%q,"sha":"%s","repo":{"id":%d}},"base":{"ref":%q,"repo":{"id":1}}}}`,
		action, number, number, head, strings.Repeat("cd", 20), headRepo, base)
}

func TestPreviews(t *testing.T) {
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
	fa := &deployAgent{}
	fa.run(conn)

	var e errBody
	base := map[string]any{"server_id": serverID, "name": "shop", "kind": "git", "git_url": "https://github.com/acme/shop.git", "git_branch": "main",
		"container_port": 3000, "domain": "shop.example.com", "builder": "railpack", "previews": true, "env": []map[string]string{{"key": "A", "value": "1"}}}
	for field, bad := range map[string]any{"builder": "nixpacks", "preview_domain": ""} {
		body := maps.Clone(base)
		body[field] = bad
		if st := admin.do("POST", "/api/v1/apps", body, &e); st != http.StatusBadRequest {
			t.Errorf("%s=%v: %d %s", field, bad, st, e.Code)
		}
	}
	base["preview_domain"] = "Preview.Example.com"
	var app struct {
		ID            string `json:"id"`
		Builder       string `json:"builder"`
		PreviewDomain string `json:"preview_domain"`
	}
	if st := admin.do("POST", "/api/v1/apps", base, &app); st != http.StatusCreated || app.Builder != "railpack" || app.PreviewDomain != "preview.example.com" {
		t.Fatalf("create: %d %+v", st, app)
	}
	var info struct {
		WebhookSecret string `json:"webhook_secret"`
	}
	admin.do("GET", "/api/v1/apps/"+app.ID+"/git", nil, &info)
	hook := p.web.URL + "/api/v1/hooks/git/" + app.ID
	type preview struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Domain        string `json:"domain"`
		State         string `json:"state"`
		GitBranch     string `json:"git_branch"`
		PreviewOf     string `json:"preview_of"`
		PreviewNumber int    `json:"preview_number"`
		Previews      bool   `json:"previews"`
		Builder       string `json:"builder"`
		Env           []struct{ Key, Value string }
	}
	previews := func() []preview {
		var list []preview
		admin.do("GET", "/api/v1/apps/"+app.ID+"/previews", nil, &list)
		return list
	}

	// Opening a pull request to the branch builds its ref in an app of its own.
	if st := signed(t, hook, info.WebhookSecret, "pull_request", prEvent("opened", 7, "feature/cart", "main", false)); st != http.StatusAccepted {
		t.Fatalf("opened: %d", st)
	}
	list := previews()
	if len(list) != 1 {
		t.Fatalf("previews: %+v", list)
	}
	pv := list[0]
	if pv.Name != "shop-pr7" || pv.Domain != "shop-pr7.preview.example.com" || pv.PreviewOf != app.ID || pv.PreviewNumber != 7 ||
		pv.GitBranch != "feature/cart" || pv.Previews || pv.Builder != "railpack" || len(pv.Env) != 1 {
		t.Fatalf("preview app: %+v", pv)
	}
	var deps []deployment
	admin.do("GET", "/api/v1/apps/"+pv.ID+"/deployments", nil, &deps)
	if len(deps) != 1 || waitDeployment(t, admin, pv.ID, deps[0].ID).Status != "live" {
		t.Fatalf("preview deploy: %+v", deps)
	}
	fa.mu.Lock()
	b := fa.builds[len(fa.builds)-1]
	fa.mu.Unlock()
	if b.Ref != "refs/pull/7/head" || b.Branch != "feature/cart" || b.Builder != "railpack" || b.Commit != strings.Repeat("cd", 20) || b.App != "shop-pr7" {
		t.Fatalf("preview build: %+v", b)
	}

	// New commits rebuild the same preview; forks, other branches and edits do not.
	signed(t, hook, info.WebhookSecret, "pull_request", prEvent("synchronize", 7, "feature/cart", "main", false))
	admin.do("GET", "/api/v1/apps/"+pv.ID+"/deployments", nil, &deps)
	if len(deps) != 2 || len(previews()) != 1 {
		t.Fatalf("synchronize: %d deployments, %d previews", len(deps), len(previews()))
	}
	waitDeployment(t, admin, pv.ID, deps[0].ID)
	for _, body := range []string{
		prEvent("opened", 8, "evil", "main", true),
		prEvent("opened", 9, "feature/x", "develop", false),
		prEvent("edited", 7, "feature/cart", "main", false),
		`{"object_attributes":{"iid":10,"action":"update","source_branch":"x","target_branch":"main","source_project_id":1,"target_project_id":1}}`,
	} {
		event := "pull_request"
		if strings.Contains(body, "object_attributes") {
			event = "Merge Request Hook"
		}
		signed(t, hook, "", event, body, "X-Gitlab-Event", event, "X-Gitlab-Token", info.WebhookSecret)
		signed(t, hook, info.WebhookSecret, event, body)
	}
	if n := len(previews()); n != 1 {
		t.Fatalf("forks, other branches and edits must not make previews: %d", n)
	}

	// GitLab merge requests too.
	mr := `{"object_attributes":{"iid":11,"action":"open","source_branch":"fix","target_branch":"main","source_project_id":5,"target_project_id":5,"last_commit":{"id":"` + strings.Repeat("ef", 20) + `"}}}`
	if st := signed(t, hook, "", "", mr, "X-Gitlab-Event", "Merge Request Hook", "X-Gitlab-Token", info.WebhookSecret); st != http.StatusAccepted || len(previews()) != 2 {
		t.Fatalf("merge request: %d %d", st, len(previews()))
	}
	admin.do("GET", "/api/v1/apps/"+previews()[1].ID+"/deployments", nil, &deps)
	waitDeployment(t, admin, previews()[1].ID, deps[0].ID)
	fa.mu.Lock()
	if b := fa.builds[len(fa.builds)-1]; b.Ref != "refs/merge-requests/11/head" {
		t.Errorf("merge request ref: %+v", b)
	}
	fa.mu.Unlock()

	// At most five at a time.
	for n := 20; n < 25; n++ {
		signed(t, hook, info.WebhookSecret, "pull_request", prEvent("opened", n, fmt.Sprintf("f%d", n), "main", false))
	}
	if n := len(previews()); n != 5 {
		t.Fatalf("previews after many pull requests: %d", n)
	}

	// Closing removes the preview, with its data.
	if st := signed(t, hook, info.WebhookSecret, "pull_request", prEvent("closed", 7, "feature/cart", "main", false)); st != http.StatusAccepted {
		t.Fatalf("closed: %d", st)
	}
	var gone preview
	admin.do("GET", "/api/v1/apps/"+pv.ID, nil, &gone)
	if gone.State != "deleting" {
		t.Fatalf("closed pull request: %+v", gone)
	}

	// Deleting the app deletes its previews.
	admin.do("DELETE", "/api/v1/apps/"+app.ID, nil, nil)
	for _, p := range previews() {
		if p.State != "deleting" {
			t.Errorf("preview %s left behind: %s", p.Name, p.State)
		}
	}

	// Customers have Git apps when their package includes them.
	var pkg, pkg2 struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Dev", "max_apps": 2, "git_apps": true}, &pkg)
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_apps": 2}, &pkg2)
	var c, c2 struct {
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "dev@example.com", "name": "Dev", "package_id": pkg.ID}, &c)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "basic@example.com", "name": "Basic", "package_id": pkg2.ID}, &c2)
	dev, basic := login(t, p.web.URL, "dev@example.com", c.Password), login(t, p.web.URL, "basic@example.com", c2.Password)
	body := maps.Clone(base)
	body["name"], body["domain"], body["previews"] = "mine", "mine.example.com", false
	if st := basic.do("POST", "/api/v1/apps", body, &e); st != http.StatusForbidden || e.Code != "git_not_allowed" {
		t.Errorf("customer without Git: %d %s", st, e.Code)
	}
	var mine struct {
		ID      string `json:"id"`
		OwnerID string `json:"owner_id"`
	}
	if st := dev.do("POST", "/api/v1/apps", body, &mine); st != http.StatusCreated || mine.OwnerID == "" {
		t.Fatalf("customer with Git: %d %+v", st, mine)
	}
	var d deployment
	if st := dev.do("POST", "/api/v1/apps/"+mine.ID+"/deploy", nil, &d); st != http.StatusAccepted {
		t.Fatalf("customer deploy: %d", st)
	}
	waitDeployment(t, dev, mine.ID, d.ID)
	fa.mu.Lock()
	if b := fa.builds[len(fa.builds)-1]; b.App != "mine" || b.Owner != mine.OwnerID {
		t.Errorf("customer build must run in their slice: %+v", b)
	}
	fa.mu.Unlock()
}
