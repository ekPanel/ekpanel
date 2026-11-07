package api

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/ssh"

	"ekpanel/internal/store"
)

var (
	// The agent checks the same: https or SSH addresses, no passwords in URLs, no local paths.
	gitRepoRE   = regexp.MustCompile(`^(https://[A-Za-z0-9.-]+(:[0-9]{1,5})?/|ssh://[A-Za-z0-9._-]+@[A-Za-z0-9.-]+(:[0-9]{1,5})?/|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:)[A-Za-z0-9._~/-]+$`)
	gitBranchRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	repoPathRE  = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,250}$`)
	commitRE    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// cleanRepoPath tidies a path inside a repository; "" means it is not one.
func cleanRepoPath(p, fallback string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return fallback
	}
	p = path.Clean(strings.TrimPrefix(p, "./"))
	if !repoPathRE.MatchString(p) || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") {
		return ""
	}
	return p
}

// newGitSecrets makes a Git app's webhook secret and deploy key, sealed for storage.
func (s *Server) newGitSecrets(n *store.NewApp) error {
	secret := make([]byte, 32)
	rand.Read(secret)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	block, err := ssh.MarshalPrivateKey(priv, "ekpanel-"+n.Name)
	if err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " ekpanel-" + n.Name
	n.WebhookSecret = s.opts.Secrets.Seal([]byte(hex.EncodeToString(secret)))
	n.DeployKey = s.opts.Secrets.Seal(pem.EncodeToMemory(block))
	n.DeployKeyPublic = &public
	return nil
}

func deploymentID(r *http.Request) string {
	if id := chi.URLParam(r, "did"); isUUID(id) {
		return id
	}
	return nilUUID
}

// gitApp loads the app in the URL and answers for anything that is not a Git app.
func (s *Server) gitApp(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return app, false
	}
	if app.Kind != "git" {
		writeError(w, http.StatusConflict, "not_git_app", "only Git apps are deployed")
		return app, false
	}
	return app, true
}

func (s *Server) handleGitInfo(w http.ResponseWriter, r *http.Request) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	secret, err := s.opts.Secrets.Open(app.WebhookSecret)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"webhook_url":       "https://" + r.Host + "/api/v1/hooks/git/" + app.ID,
		"webhook_secret":    string(secret),
		"deploy_key_public": app.DeployKeyPublic,
	})
}

func (s *Server) startDeploy(r *http.Request, app store.App, n store.NewDeployment, actor *string) (store.Deployment, error) {
	d, err := s.store.CreateDeployment(r.Context(), n)
	if err != nil {
		return d, err
	}
	if _, err := s.opts.Tasks.QueueDeploy(r.Context(), app, d, actor); err != nil {
		return d, err
	}
	return d, nil
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	if app.State == "deleting" {
		writeError(w, http.StatusConflict, "app_not_found", "the app is being deleted")
		return
	}
	u := currentUser(r)
	d, err := s.startDeploy(r, app, store.NewDeployment{AppID: app.ID, Trigger: "manual", Branch: *app.GitBranch, CreatedBy: &u.ID}, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "app.deploy_started", app.Name, map[string]any{"app_id": app.ID, "deployment": d.Number})
	writeJSON(w, http.StatusAccepted, d)
}

func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListDeployments(r.Context(), app.ID, 30)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// appDeployment loads a deployment of the app in the URL.
func (s *Server) appDeployment(w http.ResponseWriter, r *http.Request) (store.App, store.Deployment, bool) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return app, store.Deployment{}, false
	}
	d, err := s.store.GetDeployment(r.Context(), deploymentID(r))
	if err != nil || d.AppID != app.ID {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, err)
		} else {
			writeError(w, http.StatusNotFound, "deployment_not_found", "no deployment with that ID")
		}
		return app, d, false
	}
	return app, d, true
}

func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	if _, d, ok := s.appDeployment(w, r); ok {
		writeJSON(w, http.StatusOK, d)
	}
}

// handleDeploymentLog returns the log from ?offset= on, so a page following a build fetches
// only what is new.
func (s *Server) handleDeploymentLog(w http.ResponseWriter, r *http.Request) {
	_, d, ok := s.appDeployment(w, r)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	text, length, err := s.store.DeploymentLog(r.Context(), d.ID, offset)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"log": text, "length": length, "status": d.Status})
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	app, d, ok := s.appDeployment(w, r)
	if !ok {
		return
	}
	if !d.RollbackPossible {
		writeError(w, http.StatusConflict, "rollback_not_possible", "only the last five earlier releases can be returned to")
		return
	}
	u := currentUser(r)
	n := store.NewDeployment{AppID: app.ID, Trigger: "rollback", Branch: d.Branch, CommitSHA: d.CommitSHA, CommitMessage: d.CommitMessage,
		Image: d.Image, RollbackOf: &d.ID, CreatedBy: &u.ID}
	nd, err := s.startDeploy(r, app, n, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "app.rolled_back", app.Name, map[string]any{"app_id": app.ID, "deployment": nd.Number, "to": d.Number})
	writeJSON(w, http.StatusAccepted, nd)
}

// ---- Webhooks ----

// handleGitHook starts a deploy when GitHub, GitLab, Gitea or Forgejo reports a push to the
// app's branch. It needs no session: the request is signed with the app's webhook secret.
func (s *Server) handleGitHook(w http.ResponseWriter, r *http.Request) {
	app, err := s.store.GetApp(r.Context(), appID(r))
	key := "hook:" + appID(r)
	if err != nil || app.Kind != "git" || s.limiter.Blocked(key) {
		writeError(w, http.StatusNotFound, "not_found", "no such webhook")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "the request is too large")
		return
	}
	secret, err := s.opts.Secrets.Open(app.WebhookSecret)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !hookSigned(r, body, secret) {
		s.limiter.Fail(key)
		writeError(w, http.StatusUnauthorized, "invalid_signature", "the webhook's secret does not match")
		return
	}
	event := r.Header.Get("X-GitHub-Event") + r.Header.Get("X-Gitea-Event") + r.Header.Get("X-Gogs-Event") + r.Header.Get("X-Gitlab-Event")
	if event == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if event == "pull_request" || event == "Merge Request Hook" {
		s.handlePullRequest(w, r, app, event, body)
		return
	}
	var push struct {
		Ref     string `json:"ref"`
		After   string `json:"after"`
		Deleted bool   `json:"deleted"`
	}
	if event != "push" && event != "Push Hook" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "not a push"})
		return
	}
	if json.Unmarshal(body, &push) != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "the body is not a push event")
		return
	}
	branch, isBranch := strings.CutPrefix(push.Ref, "refs/heads/")
	switch {
	case !isBranch || branch != *app.GitBranch:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "another branch"})
		return
	case push.Deleted || strings.Trim(push.After, "0") == "":
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "the branch was deleted"})
		return
	case !app.AutoDeploy:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "automatic deploys are off"})
		return
	case app.State == "deleting":
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": "the app is being deleted"})
		return
	}
	n := store.NewDeployment{AppID: app.ID, Trigger: "webhook", Branch: branch}
	if commitRE.MatchString(push.After) {
		n.CommitSHA = &push.After
	}
	d, err := s.startDeploy(r, app, n, nil)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.auditAs(r, nil, "app.deploy_started", app.Name, map[string]any{"app_id": app.ID, "deployment": d.Number, "trigger": "webhook"})
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "deploying", "deployment": d.Number})
}

// hookSigned checks the signature of each forge: an HMAC-SHA256 of the body (GitHub,
// Gitea, Forgejo, Gogs) or the secret itself in a header (GitLab).
func hookSigned(r *http.Request, body, secret []byte) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	if sig == "" {
		sig = r.Header.Get("X-Gitea-Signature")
	}
	if sig == "" {
		sig = r.Header.Get("X-Gogs-Signature")
	}
	if sig != "" {
		return hmac.Equal([]byte(strings.ToLower(sig)), []byte(want))
	}
	if token := r.Header.Get("X-Gitlab-Token"); token != "" {
		return subtle.ConstantTimeCompare([]byte(token), secret) == 1
	}
	return false
}
