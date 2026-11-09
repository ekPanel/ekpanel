package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

// fakeContainers plays Docker for Git releases: running containers and the images they run.
type fakeContainers struct {
	running []string
	hash    string            // what `compose config --hash` answers
	labels  map[string]string // container -> its config hash
	image   map[string]string // container or image reference -> image ID
	images  []string          // `docker images` of the app
	next    int
	ready   error // what waitReady answers
	readyAt string
}

func gitDocker(t *testing.T, f *fakeSystem) (*fakeContainers, *[]command) {
	d := &fakeContainers{image: map[string]string{}, labels: map[string]string{}, hash: "h1"}
	calls := fakeDocker(f)
	exec := f.system.exec
	f.system.exec = func(ctx context.Context, c command) ([]byte, error) {
		if i := slices.Index(c.args, "--scale"); i >= 0 {
			d.next++
			id := "c" + string(rune('0'+d.next))
			d.running = append(d.running, id)
			d.labels[id] = d.hash
		}
		if slices.Contains(c.args, "--wait") && !slices.Contains(c.args, "--scale") && len(d.running) == 0 {
			d.running = append(d.running, "c0")
			d.labels["c0"] = d.hash
		}
		if slices.Contains(c.args, "--hash") {
			return []byte("app " + d.hash + "\n"), nil
		}
		return exec(ctx, c)
	}
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name != "docker" {
			return run(ctx, name, args...)
		}
		f.calls = append(f.calls, name+" "+strings.Join(args, " "))
		switch args[0] {
		case "ps":
			return []byte(strings.Join(d.running, "\n")), nil
		case "inspect":
			id := args[len(args)-1]
			if strings.Contains(args[2], ".State.Status") {
				return []byte("running 0 "), nil
			}
			return []byte(d.labels[id]), nil
		case "port":
			if args[1] == "c0" {
				return []byte("127.0.0.1:20010\n"), nil
			}
			return []byte("127.0.0.1:20011\n"), nil
		case "images":
			return []byte(strings.Join(d.images, "\n")), nil
		case "stop", "rm":
			d.running = slices.DeleteFunc(d.running, func(c string) bool { return c == args[len(args)-1] })
		}
		return nil, nil
	}
	prev := waitReady
	waitReady = func(_ context.Context, addr string) error { d.readyAt = addr; return d.ready }
	t.Cleanup(func() { waitReady = prev })
	return d, calls
}

func gitApp() agentproto.AppSpec {
	return agentproto.AppSpec{ID: appID, Generation: 1, Name: "shop", Kind: "git", Service: agentproto.AppServiceName,
		ContainerPort: 3000, HostPort: 20010, Domain: "shop.example.com", TLS: "auto", Volumes: []string{"/data"}}
}

func applyOne(t *testing.T, f *fakeSystem, ap agentproto.AppSpec) agentproto.AppResult {
	t.Helper()
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Apps: []agentproto.AppSpec{ap}})
	if opErr != nil || len(res.Apps) != 1 {
		t.Fatalf("apply: %v %+v", opErr, res)
	}
	return res.Apps[0]
}

func TestGitRelease(t *testing.T) {
	f := newFakeSystem(t)
	d, calls := gitDocker(t, f)
	caddy := filepath.Join(f.root, appCaddyPath(appID))

	// Not deployed yet: nothing runs and the domain is not served.
	ap := gitApp()
	if r := applyOne(t, f, ap); !r.OK || len(*calls) != 0 {
		t.Fatalf("undeployed: %+v %q", r, composeVerbs(*calls))
	}
	if _, err := os.Stat(caddy); err == nil {
		t.Fatal("an undeployed app has no web configuration")
	}

	// First release: a plain start, on a range of two ports.
	ap.Image, ap.Generation = "ekpanel/shop:aaa111", 2
	if r := applyOne(t, f, ap); !r.OK || !slices.Contains(composeVerbs(*calls), "up --detach --remove-orphans --wait --wait-timeout 120") {
		t.Fatalf("first release: %+v %q", r, composeVerbs(*calls))
	}
	file, _ := os.ReadFile(filepath.Join(f.root, appsDir, "shop", appComposeFile))
	if !strings.Contains(string(file), `"127.0.0.1:20010-20011:3000"`) || !strings.Contains(string(file), `"image": "ekpanel/shop:aaa111"`) ||
		!strings.Contains(string(file), `"PORT": "3000"`) {
		t.Fatalf("compose file:\n%s", file)
	}
	conf, _ := os.ReadFile(caddy)
	if !strings.Contains(string(conf), "reverse_proxy 127.0.0.1:20010 127.0.0.1:20011 {") || !strings.Contains(string(conf), "lb_policy first") {
		t.Fatalf("caddy file:\n%s", conf)
	}
	d.running, d.labels["c0"] = []string{"c0"}, "h1"

	// Nothing changed: the running container stays.
	*calls = nil
	if r := applyOne(t, f, ap); !r.OK || !slices.Contains(composeVerbs(*calls), "up --detach --no-recreate") {
		t.Fatalf("unchanged: %+v %q", r, composeVerbs(*calls))
	}

	// A new build: a second container starts, and the first stops once it answers.
	*calls, f.calls = nil, nil
	ap.Image, ap.Generation, ap.KeepImages = "ekpanel/shop:bbb222", 3, []string{"ekpanel/shop:bbb222", "ekpanel/shop:aaa111"}
	d.hash = "h2"
	d.images = []string{"ekpanel/shop:bbb222", "ekpanel/shop:aaa111", "ekpanel/shop:ccc333", "ekpanel/other:x"}
	if r := applyOne(t, f, ap); !r.OK {
		t.Fatalf("release: %+v", r)
	}
	if !slices.Contains(composeVerbs(*calls), "up --detach --no-deps --no-recreate --scale app=2 app") || d.readyAt != "127.0.0.1:20011" {
		t.Fatalf("rolling start: %q, waited on %q", composeVerbs(*calls), d.readyAt)
	}
	if !slices.Equal(d.running, []string{"c1"}) || !slices.Contains(f.calls, "docker stop --time 30 c0") {
		t.Fatalf("the old container must stop after the new one is ready: %v %q", d.running, f.calls)
	}
	if conf, _ := os.ReadFile(caddy); !strings.Contains(string(conf), "reverse_proxy 127.0.0.1:20011 127.0.0.1:20010 {") {
		t.Fatalf("the running release's port goes first:\n%s", conf)
	}
	if !slices.Contains(f.calls, "docker rmi ekpanel/shop:ccc333") || slices.ContainsFunc(f.calls, func(c string) bool {
		return c == "docker rmi ekpanel/shop:aaa111" || strings.Contains(c, "other")
	}) {
		t.Fatalf("only builds that are not kept go: %q", f.calls)
	}

	// A build that does not start: it is removed, and the running one keeps serving.
	f.calls = nil
	d.ready = errors.New("it did not answer within 3 minutes")
	ap.Image, ap.Generation, d.hash = "ekpanel/shop:ddd444", 4, "h3"
	r := applyOne(t, f, ap)
	if r.OK || !strings.Contains(r.Error, "previous one keeps running") {
		t.Fatalf("failed release: %+v", r)
	}
	if !slices.Equal(d.running, []string{"c1"}) || !slices.Contains(f.calls, "docker rm --force c2") || slices.Contains(f.calls, "docker stop --time 30 c1") {
		t.Fatalf("the failed container goes, the old one stays: %v %q", d.running, f.calls)
	}

	// Going back to what runs (as the panel does after a failed release) restarts nothing.
	*calls, f.calls, d.ready = nil, nil, nil
	ap.Image, ap.Generation, d.hash = "ekpanel/shop:bbb222", 5, "h2"
	if r := applyOne(t, f, ap); !r.OK || slices.ContainsFunc(composeVerbs(*calls), func(v string) bool { return strings.Contains(v, "--scale") }) {
		t.Fatalf("back to the running build: %+v %q", r, composeVerbs(*calls))
	}

	// Deleting removes the app's builds too.
	f.calls = nil
	d.images = []string{"img1", "img2"}
	ap.Deleted, ap.Generation = true, 6
	if r := applyOne(t, f, ap); !r.OK || !slices.Contains(f.calls, "docker rmi --force img1 img2") {
		t.Fatalf("delete: %+v %q", r, f.calls)
	}
}

func TestGitFirstReleaseFails(t *testing.T) {
	f := newFakeSystem(t)
	d, calls := gitDocker(t, f)
	d.ready = errors.New("it did not answer within 3 minutes")
	ap := gitApp()
	ap.Image, ap.Generation = "ekpanel/shop:aaa111", 2
	if r := applyOne(t, f, ap); r.OK || !strings.Contains(r.Error, "the app did not start") || !slices.Contains(f.calls, "docker rm --force c0") {
		t.Fatalf("a first release that does not start must fail and go: %+v %q", r, f.calls)
	}
	// The panel then goes back to no build: whatever is left stops, the domain goes.
	*calls = nil
	ap.Image, ap.Generation = "", 3
	if r := applyOne(t, f, ap); !r.OK || !slices.Contains(composeVerbs(*calls), "down --remove-orphans") {
		t.Fatalf("back to undeployed: %+v %q", r, composeVerbs(*calls))
	}
	if _, err := os.Stat(filepath.Join(f.root, appsDir, "shop", appComposeFile)); err == nil {
		t.Error("the Compose file of a build that is gone must go")
	}
}

func TestValidateGitApp(t *testing.T) {
	ok := gitApp()
	ok.Image = "ekpanel/shop:abc123"
	if err := validateApp(ok); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*agentproto.AppSpec){
		func(a *agentproto.AppSpec) { a.Image = "ekpanel/other:abc123" },
		func(a *agentproto.AppSpec) { a.Image = "nginx:latest" },
		func(a *agentproto.AppSpec) { a.KeepImages = []string{"alpine"} },
		func(a *agentproto.AppSpec) { a.ContainerPort = 0 },
		func(a *agentproto.AppSpec) { a.HostPort = 29999 },
		func(a *agentproto.AppSpec) { a.Volumes = []string{"/data/../../etc"} },
	} {
		ap := ok
		change(&ap)
		if err := validateApp(ap); err == nil {
			t.Errorf("accepted %+v", ap)
		}
	}
}

func TestValidateBuild(t *testing.T) {
	ok := agentproto.BuildParams{Stream: "s1", App: "shop", Image: "ekpanel/shop:abc", Repo: "https://github.com/acme/shop.git",
		Branch: "main", Builder: "dockerfile", Dockerfile: "Dockerfile", Context: "."}
	for _, repo := range []string{"https://github.com/acme/shop.git", "git@github.com:acme/shop.git", "ssh://git@git.example.com:2222/acme/shop.git", "https://gitea.example.com/acme/shop"} {
		p := ok
		p.Repo = repo
		if err := validateBuild(p); err != nil {
			t.Errorf("%s: %v", repo, err)
		}
	}
	for _, change := range []func(*agentproto.BuildParams){
		func(p *agentproto.BuildParams) { p.Repo = "file:///etc" },
		func(p *agentproto.BuildParams) { p.Repo = "/srv/repo" },
		func(p *agentproto.BuildParams) { p.Repo = "ext::sh -c id" },
		func(p *agentproto.BuildParams) { p.Repo = "https://user:token@github.com/acme/shop" },
		func(p *agentproto.BuildParams) { p.Repo = "--upload-pack=touch /tmp/x" },
		func(p *agentproto.BuildParams) { p.Repo = "https://github.com/acme/../../x" },
		func(p *agentproto.BuildParams) { p.Branch = "-x" },
		func(p *agentproto.BuildParams) { p.Branch = "a..b" },
		func(p *agentproto.BuildParams) { p.Commit = "HEAD~1" },
		func(p *agentproto.BuildParams) { p.Dockerfile = "../etc/passwd" },
		func(p *agentproto.BuildParams) { p.Context = "/" },
		func(p *agentproto.BuildParams) { p.Image = "nginx:latest" },
		func(p *agentproto.BuildParams) { p.Env = []agentproto.EnvVar{{Key: "BAD KEY"}} },
		func(p *agentproto.BuildParams) { p.Builder = "nixpacks" },
		func(p *agentproto.BuildParams) { p.Ref = "refs/heads/main" },
		func(p *agentproto.BuildParams) { p.Ref = "refs/pull/7/head; rm -rf /" },
	} {
		p := ok
		change(&p)
		if err := validateBuild(p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

func TestRunBuild(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["git"] = true
	var calls []command
	var keyDuringClone string
	f.system.exec = func(_ context.Context, c command) ([]byte, error) {
		calls = append(calls, c)
		args := strings.Join(c.args, " ")
		switch {
		case c.name == "git" && strings.Contains(args, " clone "):
			src := c.args[len(c.args)-1]
			os.MkdirAll(filepath.Join(src, "app"), 0o755)
			os.WriteFile(filepath.Join(src, "app", "Dockerfile"), []byte("FROM alpine\n"), 0o644)
			os.Symlink("/etc/passwd", filepath.Join(src, "evil"))
			for _, e := range c.env {
				if s, ok := strings.CutPrefix(e, "GIT_SSH_COMMAND="); ok {
					key, _ := os.ReadFile(s[strings.LastIndex(s, " ")+1:])
					keyDuringClone = string(key)
				}
			}
			c.stderr.Write([]byte("Cloning into 'src'...\n"))
		case c.name == "git" && strings.Contains(args, " fetch --depth 1 origin bad"):
			return nil, errors.New("not our ref")
		case c.name == "git" && strings.Contains(args, " log "):
			return []byte("0123456789abcdef0123456789abcdef01234567\nAdd the shop\n"), nil
		case c.name == "docker":
			c.stdout.Write([]byte("#1 DONE\n"))
		}
		return nil, nil
	}
	var out []byte
	w := &streamWriter{id: "s1", send: func(env agentproto.Envelope) error { out = append(out, env.Data...); return nil }}
	p := agentproto.BuildParams{Stream: "s1", App: "shop", Image: "ekpanel/shop:abc", Repo: "git@github.com:acme/shop.git", Branch: "main", Builder: "auto",
		Dockerfile: "app/Dockerfile", Context: "app", SSHKey: "-----BEGIN OPENSSH PRIVATE KEY-----\nxyz\n-----END OPENSSH PRIVATE KEY-----",
		Env: []agentproto.EnvVar{{Key: "TOKEN", Value: "s3cret"}}}
	res, err := runBuild(context.Background(), f.system, p, w)
	if err != nil || res.Commit != "0123456789abcdef0123456789abcdef01234567" || res.Message != "Add the shop" {
		t.Fatalf("build: %v %+v", err, res)
	}
	if !strings.Contains(keyDuringClone, "BEGIN OPENSSH PRIVATE KEY") {
		t.Error("the deploy key must be there while cloning")
	}
	if entries, _ := os.ReadDir(filepath.Join(f.root, buildsDir)); len(entries) != 0 {
		t.Error("the build folder (with the key) must be removed afterwards")
	}
	clone := strings.Join(calls[0].args, " ")
	if !strings.Contains(clone, "protocol.file.allow=never") || !strings.Contains(clone, "--branch main --recurse-submodules --shallow-submodules -- git@github.com:acme/shop.git") ||
		!slices.Contains(calls[0].env, "GIT_ALLOW_PROTOCOL=https:ssh") {
		t.Errorf("clone: %s %v", clone, calls[0].env)
	}
	build := calls[len(calls)-1]
	args := strings.Join(build.args, " ")
	if !strings.Contains(args, "--tag ekpanel/shop:abc") || !strings.Contains(args, "--build-arg TOKEN") || strings.Contains(args, "s3cret") ||
		!slices.Contains(build.env, "TOKEN=s3cret") || !strings.HasSuffix(args, "/src/app") || !strings.Contains(args, "/src/app/Dockerfile") {
		t.Errorf("docker build: %s %v", args, build.env)
	}
	if !strings.Contains(string(out), "==> Cloning git@github.com:acme/shop.git, branch main") || !strings.Contains(string(out), "#1 DONE") {
		t.Errorf("output:\n%s", out)
	}

	// A Dockerfile that leads outside the repository is refused.
	p.Dockerfile = "evil"
	if _, err := runBuild(context.Background(), f.system, p, w); err == nil || !strings.Contains(err.Error(), "outside the repository") {
		t.Errorf("link out of the repository: %v", err)
	}

	// A wanted commit that is not the branch's head is fetched.
	calls = nil
	p.Dockerfile, p.Commit = "app/Dockerfile", "fedcba9876543210"
	runBuild(context.Background(), f.system, p, w)
	var verbs []string
	for _, c := range calls {
		if c.name == "git" {
			verbs = append(verbs, strings.Join(c.args[4:], " "))
		}
	}
	if !slices.ContainsFunc(verbs, func(v string) bool { return strings.HasSuffix(v, "fetch --depth 1 origin fedcba9876543210") }) ||
		!slices.ContainsFunc(verbs, func(v string) bool { return strings.HasSuffix(v, "checkout --quiet --detach fedcba9876543210") }) {
		t.Errorf("commit not fetched: %q", verbs)
	}
	// A server that does not give out single commits: more of the branch is fetched.
	calls = nil
	p.Commit = "bad0123"
	runBuild(context.Background(), f.system, p, w)
	verbs = nil
	for _, c := range calls {
		if c.name == "git" {
			verbs = append(verbs, strings.Join(c.args[4:], " "))
		}
	}
	if !slices.ContainsFunc(verbs, func(v string) bool { return strings.HasSuffix(v, "fetch --depth 200 origin main") }) {
		t.Errorf("history not deepened: %q", verbs)
	}
}

// fakeRailpack makes a release archive holding a railpack program, and trusts it.
func fakeRailpack(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	program := []byte("#!/bin/sh\n")
	tw.WriteHeader(&tar.Header{Name: "railpack-v0/railpack", Mode: 0o755, Size: int64(len(program)), Typeflag: tar.TypeReg})
	tw.Write(program)
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	prev := railpackSHA256[runtime.GOARCH]
	railpackSHA256[runtime.GOARCH] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { railpackSHA256[runtime.GOARCH] = prev })
	return buf.Bytes()
}

func TestRailpackBuild(t *testing.T) {
	f := newFakeSystem(t)
	f.installed["git"] = true
	archive := fakeRailpack(t)
	fetched := 0
	f.system.fetch = func(_ context.Context, url string) ([]byte, error) {
		fetched++
		if !strings.Contains(url, "/v"+railpackVersion+"/railpack-v"+railpackVersion+"-") {
			t.Errorf("download %s", url)
		}
		return archive, nil
	}
	var calls []command
	f.system.exec = func(_ context.Context, c command) ([]byte, error) {
		calls = append(calls, c)
		args := strings.Join(c.args, " ")
		switch {
		case c.name == "git" && (strings.Contains(args, " clone ") || strings.Contains(args, " init ")):
			src := c.args[len(c.args)-1]
			os.MkdirAll(src, 0o755)
			os.WriteFile(filepath.Join(src, "package.json"), []byte("{}"), 0o644)
		case c.name == "git" && strings.Contains(args, " log "):
			return []byte("0123456789abcdef0123456789abcdef01234567\nNo Dockerfile here\n"), nil
		}
		return nil, nil
	}
	w := &streamWriter{id: "s1", send: func(agentproto.Envelope) error { return nil }}
	p := agentproto.BuildParams{Stream: "s1", App: "web", Image: "ekpanel/web:abc", Repo: "https://github.com/acme/web.git", Branch: "main",
		Builder: "auto", Dockerfile: "Dockerfile", Context: ".", Env: []agentproto.EnvVar{{Key: "TOKEN", Value: "s3cret"}}}
	if _, err := runBuild(context.Background(), f.system, p, w); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(f.root, railpackDir, "railpack-"+railpackVersion)
	if info, err := os.Stat(bin); err != nil || info.Mode().Perm() != 0o755 || fetched != 1 {
		t.Fatalf("railpack installed: %v %v, fetched %d", info, err, fetched)
	}
	var prepare, build command
	for _, c := range calls {
		if c.name == bin {
			prepare = c
		}
		if c.name == "docker" {
			build = c
		}
	}
	if args := strings.Join(prepare.args, " "); !strings.HasPrefix(args, "prepare ") || !strings.HasSuffix(args, "--env TOKEN") || strings.Contains(args, "s3cret") {
		t.Errorf("prepare: %s", args)
	}
	args := strings.Join(build.args, " ")
	if !strings.Contains(args, "BUILDKIT_SYNTAX="+railpackFrontend) || !strings.Contains(args, "--secret id=TOKEN,env=TOKEN") ||
		!strings.Contains(args, "--tag ekpanel/web:abc") || !strings.Contains(args, "--file "+filepath.Join(f.root, buildsDir)) ||
		strings.Contains(args, "s3cret") || !slices.Contains(build.env, "TOKEN=s3cret") || !slices.Contains(prepare.env, "TOKEN=s3cret") {
		t.Errorf("railpack build: %s %v", args, build.env)
	}

	// Railpack is downloaded once.
	runBuild(context.Background(), f.system, p, w)
	if fetched != 1 {
		t.Errorf("downloaded %d times", fetched)
	}

	// A download that does not match its checksum is refused.
	os.Remove(bin)
	railpackSHA256[runtime.GOARCH] = strings.Repeat("0", 64)
	if _, err := runBuild(context.Background(), f.system, p, w); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("bad checksum: %v", err)
	}

	// A pull request's ref is fetched instead of the branch.
	calls = nil
	p.Builder, p.Ref = "dockerfile", "refs/pull/7/head"
	runBuild(context.Background(), f.system, p, w)
	var verbs []string
	for _, c := range calls {
		if c.name == "git" {
			verbs = append(verbs, strings.Join(c.args[4:], " "))
		}
	}
	if !slices.ContainsFunc(verbs, func(v string) bool { return strings.HasSuffix(v, "fetch --depth 1 origin refs/pull/7/head") }) ||
		slices.ContainsFunc(verbs, func(v string) bool { return strings.HasPrefix(v, "clone") }) {
		t.Errorf("pull request fetch: %q", verbs)
	}
}
