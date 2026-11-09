package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"ekpanel/internal/agentproto"
)

// Builds turn a Git app's repository into an image on this server. The output streams to
// the panel as it happens; the build belongs to the connection that asked for it.

const (
	buildsDir      = "/srv/ekpanel/builds"
	knownHostsFile = "/srv/ekpanel/known_hosts"
	maxBuilds      = 2
	buildTimeout   = time.Hour
)

var (
	// https URLs, ssh:// URLs and scp-like addresses (git@host:owner/repo.git). No passwords
	// in URLs, no local paths and no other transports.
	repoRE     = regexp.MustCompile(`^(https://[A-Za-z0-9.-]+(:[0-9]{1,5})?/|ssh://[A-Za-z0-9._-]+@[A-Za-z0-9.-]+(:[0-9]{1,5})?/|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:)[A-Za-z0-9._~/-]+$`)
	branchRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	commitRE   = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
	repoPathRE = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,250}$`)
	buildTagRE = regexp.MustCompile(`^[a-z0-9]{1,32}$`)
	// The refs forges keep for pull requests (GitHub, Gitea, Forgejo) and merge requests (GitLab).
	prRefRE = regexp.MustCompile(`^refs/(pull/[0-9]{1,9}/head|merge-requests/[0-9]{1,9}/head)$`)
)

// gitImageOK says whether image is a build of the named app.
func gitImageOK(app, image string) bool {
	tag, ok := strings.CutPrefix(image, agentproto.GitImageRepo(app)+":")
	return ok && buildTagRE.MatchString(tag)
}

// repoPath checks a path inside the repository: relative, without "..".
func repoPath(p string) bool {
	if !repoPathRE.MatchString(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func validateBuild(p agentproto.BuildParams) error {
	switch {
	case !streamIDRE.MatchString(p.Stream):
		return errors.New("invalid stream id")
	case !appNameRE.MatchString(p.App) || !gitImageOK(p.App, p.Image):
		return errors.New("invalid app or image")
	case !repoRE.MatchString(p.Repo) || strings.Contains(p.Repo, ".."):
		return errors.New("the repository must be an https:// or SSH address")
	case !branchRE.MatchString(p.Branch) || strings.Contains(p.Branch, "..") || strings.HasSuffix(p.Branch, ".lock") || strings.HasSuffix(p.Branch, "/"):
		return errors.New("invalid branch name")
	case p.Commit != "" && !commitRE.MatchString(p.Commit):
		return errors.New("invalid commit")
	case !repoPath(p.Dockerfile) || !repoPath(p.Context):
		return errors.New("the Dockerfile and build folder must be paths inside the repository")
	case p.Builder != "dockerfile" && p.Builder != "railpack" && p.Builder != "auto":
		return fmt.Errorf("unknown builder %q", p.Builder)
	case p.Ref != "" && !prRefRE.MatchString(p.Ref):
		return errors.New("invalid ref")
	case p.Owner != "" && !uuidRE.MatchString(p.Owner):
		return errors.New("invalid owner")
	}
	for _, e := range p.Env {
		if !envKeyRE.MatchString(e.Key) {
			return fmt.Errorf("invalid variable %q", e.Key)
		}
	}
	return nil
}

type builds struct {
	sys  *system
	send func(agentproto.Envelope) error
	mu   sync.Mutex
	open map[string]context.CancelFunc
}

func newBuilds(sys *system, send func(agentproto.Envelope) error) *builds {
	return &builds{sys: sys, send: send, open: map[string]context.CancelFunc{}}
}

// start begins a build; its output and result arrive on the stream.
func (b *builds) start(ctx context.Context, p agentproto.BuildParams) *agentproto.OpError {
	if err := validateBuild(p); err != nil {
		return opError(agentproto.ErrInvalidParams, "%v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, taken := b.open[p.Stream]; taken {
		return opError(agentproto.ErrInvalidParams, "stream id in use")
	}
	if len(b.open) >= maxBuilds {
		return opError(agentproto.ErrNotAllowed, "two builds are already running on this server; try again when one ends")
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	b.open[p.Stream] = cancel
	go func() {
		defer cancel()
		w := &streamWriter{id: p.Stream, send: b.send}
		res, err := runBuild(ctx, b.sys, p, w)
		b.mu.Lock()
		_, still := b.open[p.Stream]
		delete(b.open, p.Stream)
		b.mu.Unlock()
		if !still {
			return // the panel closed the stream; it is not listening any more
		}
		end := agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: p.Stream, Reason: "exit"}
		if err != nil {
			end.Reason = trimText(err.Error(), 2000)
		} else {
			end.Result, _ = json.Marshal(res)
		}
		b.send(end)
	}()
	return nil
}

// close cancels a build the panel stopped listening to.
func (b *builds) close(id string) {
	b.mu.Lock()
	cancel := b.open[id]
	delete(b.open, id)
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (b *builds) closeAll() {
	b.mu.Lock()
	all := b.open
	b.open = map[string]context.CancelFunc{}
	b.mu.Unlock()
	for _, cancel := range all {
		cancel()
	}
}

// streamWriter sends what is written to it as stream data.
type streamWriter struct {
	id   string
	send func(agentproto.Envelope) error
	mu   sync.Mutex
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock() // git and docker write from two pipes
	defer w.mu.Unlock()
	for rest := p; len(rest) > 0; {
		n := min(len(rest), agentproto.MaxStreamChunk)
		if err := w.send(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: w.id, Data: append([]byte(nil), rest[:n]...)}); err != nil {
			return 0, err
		}
		rest = rest[n:]
	}
	return len(p), nil
}

func (w *streamWriter) step(format string, args ...any) {
	fmt.Fprintf(w, "\n==> "+format+"\n", args...)
}

func trimText(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// runBuild clones the repository into a folder of its own and builds the image there.
func runBuild(ctx context.Context, sys *system, p agentproto.BuildParams, w *streamWriter) (agentproto.BuildResult, error) {
	var res agentproto.BuildResult
	w.step("Preparing Docker and Git")
	if err := ensureDocker(ctx, sys); err != nil {
		return res, err
	}
	if err := ensurePackage(ctx, sys, "git"); err != nil {
		return res, err
	}
	// BuildKit gives better output and caching; without it Docker uses its older builder.
	buildx := true
	if _, err := sys.run(ctx, "docker", "buildx", "version"); err != nil {
		buildx = ensurePackage(ctx, sys, "docker-buildx") == nil
	}

	if err := os.MkdirAll(sys.path(buildsDir), 0o700); err != nil {
		return res, err
	}
	dir := sys.path(buildsDir + "/" + p.App + "-" + p.Stream)
	os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "src")

	// Git reads no configuration of this machine, never prompts, and speaks only https and
	// ssh, so a repository cannot point at local files or run helper programs.
	env := []string{"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https:ssh", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + dir}
	ssh := "ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" + sys.path(knownHostsFile)
	if p.SSHKey != "" {
		key := filepath.Join(dir, "deploy_key")
		if err := os.WriteFile(key, []byte(strings.TrimSpace(p.SSHKey)+"\n"), 0o600); err != nil {
			return res, err
		}
		ssh += " -o IdentitiesOnly=yes -i " + key
	}
	env = append(env, "GIT_SSH_COMMAND="+ssh)
	git := func(args ...string) error {
		args = append([]string{"-c", "protocol.file.allow=never", "-c", "core.hooksPath=/dev/null"}, args...)
		_, err := sys.exec(ctx, command{name: "git", args: args, env: env, stdout: w, stderr: w})
		return err
	}

	if p.Ref != "" {
		// A pull request is not a branch of the repository; fetch its ref instead.
		w.step("Fetching %s from %s", p.Ref, p.Repo)
		for _, args := range [][]string{{"init", "--quiet", src}, {"-C", src, "remote", "add", "origin", "--", p.Repo},
			{"-C", src, "fetch", "--depth", "1", "origin", p.Ref}, {"-C", src, "checkout", "--quiet", "--detach", "FETCH_HEAD"},
			{"-C", src, "submodule", "update", "--init", "--recursive", "--depth", "1"}} {
			if err := git(args...); err != nil {
				return res, fmt.Errorf("could not fetch %s: %w", p.Ref, err)
			}
		}
	} else {
		w.step("Cloning %s, branch %s", p.Repo, p.Branch)
		if err := git("clone", "--depth", "1", "--branch", p.Branch, "--recurse-submodules", "--shallow-submodules", "--", p.Repo, src); err != nil {
			return res, fmt.Errorf("could not clone the repository: %w", err)
		}
	}
	head := func() (string, string, error) {
		out, err := sys.exec(ctx, command{name: "git", args: []string{"-C", src, "log", "-1", "--format=%H%n%s"}, env: env})
		sha, msg, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return sha, msg, err
	}
	sha, msg, err := head()
	if err != nil {
		return res, err
	}
	if p.Commit != "" && p.Ref == "" && !strings.HasPrefix(sha, p.Commit) {
		// The branch moved on since the push. Forges hand out single commits; plain Git
		// servers do not, so then more of the branch's history is fetched instead.
		w.step("Fetching commit %s", p.Commit)
		if err := git("-C", src, "fetch", "--depth", "1", "origin", p.Commit); err != nil {
			w.step("Fetching the last 200 commits of %s instead", p.Branch)
			if err := git("-C", src, "fetch", "--depth", "200", "origin", p.Branch); err != nil {
				return res, fmt.Errorf("could not fetch commit %s: %w", p.Commit, err)
			}
		}
		if err := git("-C", src, "checkout", "--quiet", "--detach", p.Commit); err != nil {
			return res, fmt.Errorf("commit %s is not on branch %s: %w", p.Commit, p.Branch, err)
		}
		if err := git("-C", src, "submodule", "update", "--init", "--recursive", "--depth", "1"); err != nil {
			return res, err
		}
		if sha, msg, err = head(); err != nil {
			return res, err
		}
	}
	res.Commit, res.Message = sha, trimText(msg, 500)

	// The Dockerfile and build folder must stay inside the repository, also through links.
	inside := func(rel string) (string, error) {
		abs, err := filepath.EvalSymlinks(filepath.Join(src, rel))
		if err != nil {
			return "", fmt.Errorf("%s is not in the repository", rel)
		}
		if abs != src && !strings.HasPrefix(abs, src+"/") {
			return "", fmt.Errorf("%s points outside the repository", rel)
		}
		return abs, nil
	}
	buildDir, err := inside(p.Context)
	if err != nil {
		return res, err
	}
	builder := p.Builder
	if builder == "auto" {
		builder = "dockerfile"
		if _, err := os.Lstat(filepath.Join(src, p.Dockerfile)); errors.Is(err, os.ErrNotExist) {
			builder = "railpack"
		}
	}
	buildEnv := []string{"BUILDKIT_PROGRESS=plain"}
	for _, e := range p.Env {
		buildEnv = append(buildEnv, e.Key+"="+e.Value)
	}
	w.step("Building commit %.12s: %s", res.Commit, res.Message)
	if builder == "railpack" {
		if !buildx {
			return res, errors.New("Railpack needs Docker BuildKit (the docker-buildx package)")
		}
		if err := railpackBuild(ctx, sys, p, w, dir, buildDir, buildEnv); err != nil {
			return res, err
		}
		w.step("Built %s", p.Image)
		sys.run(ctx, "docker", "builder", "prune", "--force", "--filter", "until=168h")
		return res, nil
	}
	dockerfile, err := inside(p.Dockerfile)
	if err != nil {
		return res, err
	}
	args := []string{"build"}
	if buildx {
		args = append(args, "--progress=plain")
	}
	args = append(args, "--tag", p.Image, "--file", dockerfile, "--label", "ekpanel.app="+p.App)
	if p.Owner != "" {
		args = append(args, "--cgroup-parent", customerSlice(ownerKey(p.Owner)))
	}
	for _, e := range p.Env {
		// "--build-arg NAME" takes the value from the environment, so it is not on the command line.
		args = append(args, "--build-arg", e.Key)
	}
	args = append(args, buildDir)
	if _, err := sys.exec(ctx, command{name: "docker", args: args, env: buildEnv, stdout: w, stderr: w}); err != nil {
		return res, fmt.Errorf("the build failed: %w", err)
	}
	w.step("Built %s", p.Image)
	// Keep a week of build cache: fast rebuilds without filling the disk.
	sys.run(ctx, "docker", "builder", "prune", "--force", "--filter", "until=168h")
	return res, nil
}
