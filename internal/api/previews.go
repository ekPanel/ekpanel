package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"ekpanel/internal/store"
)

// maxPreviews is how many pull requests of one app are previewed at a time.
const maxPreviews = 5

// pullRequest is what the forges' pull request events say, in one shape.
type pullRequest struct {
	Number   int
	Action   string // "deploy", "close" or "" (nothing to do)
	Head     string // the pull request's branch
	Base     string // the branch it goes into
	SHA      string
	Ref      string // where the forge keeps the pull request's commits
	SameRepo bool   // false for pull requests from forks
}

// parsePullRequest reads a pull request event of GitHub, Gitea, Forgejo, Gogs or GitLab.
func parsePullRequest(event string, body []byte) (pullRequest, error) {
	var pr pullRequest
	if event == "Merge Request Hook" {
		var m struct {
			Attrs struct {
				IID          int    `json:"iid"`
				Action       string `json:"action"`
				Source       string `json:"source_branch"`
				Target       string `json:"target_branch"`
				SourceID     int64  `json:"source_project_id"`
				TargetID     int64  `json:"target_project_id"`
				OldRev       string `json:"oldrev"`
				LastCommitID struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return pr, err
		}
		a := m.Attrs
		pr = pullRequest{Number: a.IID, Head: a.Source, Base: a.Target, SHA: a.LastCommitID.ID,
			Ref: fmt.Sprintf("refs/merge-requests/%d/head", a.IID), SameRepo: a.SourceID != 0 && a.SourceID == a.TargetID}
		switch {
		case a.Action == "open" || a.Action == "reopen" || (a.Action == "update" && a.OldRev != ""):
			pr.Action = "deploy" // an update without oldrev changed the title or labels, not the code
		case a.Action == "close" || a.Action == "merge":
			pr.Action = "close"
		}
		return pr, nil
	}
	var e struct {
		Action string `json:"action"`
		Number int    `json:"number"`
		PR     struct {
			Number int `json:"number"`
			Head   struct {
				Ref  string `json:"ref"`
				SHA  string `json:"sha"`
				Repo *struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"head"`
			Base struct {
				Ref  string `json:"ref"`
				Repo *struct {
					ID int64 `json:"id"`
				} `json:"repo"`
			} `json:"base"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return pr, err
	}
	n := e.Number
	if n == 0 {
		n = e.PR.Number
	}
	head, base := e.PR.Head, e.PR.Base
	pr = pullRequest{Number: n, Head: head.Ref, Base: base.Ref, SHA: head.SHA, Ref: fmt.Sprintf("refs/pull/%d/head", n),
		SameRepo: head.Repo != nil && base.Repo != nil && head.Repo.ID != 0 && head.Repo.ID == base.Repo.ID}
	switch e.Action {
	case "opened", "reopened", "synchronize", "synchronized":
		pr.Action = "deploy"
	case "closed":
		pr.Action = "close"
	}
	return pr, nil
}

// previewName is the app name of a pull request's preview: the app's name, shortened if
// needed, and -pr<number>.
func previewName(app string, number int) string {
	suffix := fmt.Sprintf("-pr%d", number)
	if len(app)+len(suffix) > 32 {
		app = strings.TrimRight(app[:32-len(suffix)], "-")
	}
	return app + suffix
}

// handlePullRequest deploys a preview of a pull request to the app's branch, or removes
// it when the pull request closes. Pull requests from forks are not previewed: their code
// would run on the server.
func (s *Server) handlePullRequest(w http.ResponseWriter, r *http.Request, app store.App, event string, body []byte) {
	ignore := func(reason string) {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": reason})
	}
	pr, err := parsePullRequest(event, body)
	switch {
	case err != nil || pr.Number <= 0:
		writeError(w, http.StatusBadRequest, "invalid_json", "the body is not a pull request event")
		return
	case app.PreviewOf != nil || !app.Previews || app.PreviewDomain == nil:
		ignore("previews are off")
		return
	case pr.Action == "":
		ignore("nothing to deploy")
		return
	case pr.Base != *app.GitBranch:
		ignore("the pull request goes into another branch")
		return
	case !pr.SameRepo:
		ignore("pull requests from forks are not previewed")
		return
	case app.State == "deleting":
		ignore("the app is being deleted")
		return
	}
	previews, err := s.store.Previews(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var preview *store.App
	for i := range previews {
		if *previews[i].PreviewNumber == pr.Number && previews[i].State != "deleting" {
			preview = &previews[i]
		}
	}

	if pr.Action == "close" {
		if preview == nil {
			ignore("no preview of this pull request")
			return
		}
		if _, err := s.store.MarkAppDeleting(r.Context(), preview.ID, true); err != nil {
			s.internalError(w, r, err)
			return
		}
		s.auditAs(r, nil, "app.preview_removed", preview.Name, map[string]any{"app_id": app.ID, "pull_request": pr.Number})
		s.queueReconcile(r, app.ServerID)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "removing", "app": preview.Name})
		return
	}

	if preview == nil {
		live := 0
		for _, p := range previews {
			if p.State != "deleting" {
				live++
			}
		}
		if live >= maxPreviews {
			ignore(fmt.Sprintf("there are already %d previews; close a pull request first", maxPreviews))
			return
		}
		p, err := s.createPreview(r, app, pr)
		if err != nil {
			var taken store.ErrHostnameTaken
			if errors.Is(err, store.ErrAppNameTaken) || errors.As(err, &taken) {
				ignore(err.Error())
				return
			}
			s.internalError(w, r, err)
			return
		}
		preview = &p
		s.auditAs(r, nil, "app.preview_created", p.Name, map[string]any{"app_id": app.ID, "pull_request": pr.Number})
	}
	n := store.NewDeployment{AppID: preview.ID, Trigger: "webhook", Branch: pr.Head, Ref: &pr.Ref}
	if commitRE.MatchString(pr.SHA) {
		n.CommitSHA = &pr.SHA
	}
	d, err := s.startDeploy(r, *preview, n, nil)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "deploying", "app": preview.Name, "deployment": d.Number})
}

// createPreview makes the app of a pull request: a copy of the app's settings, on a host
// name of its own, with no previews and no automatic deploys of its own.
func (s *Server) createPreview(r *http.Request, app store.App, pr pullRequest) (store.App, error) {
	name := previewName(app.Name, pr.Number)
	domain := name + "." + *app.PreviewDomain
	n := store.NewApp{ServerID: app.ServerID, Name: name, Kind: "git", OwnerID: app.OwnerID, PreviewOf: &app.ID, PreviewNumber: &pr.Number,
		WebhookSecret: app.WebhookSecret, DeployKey: app.DeployKey, DeployKeyPublic: app.DeployKeyPublic,
		AppSettings: store.AppSettings{Service: app.Service, ContainerPort: app.ContainerPort, Domain: &domain, TLS: app.TLS,
			Env: app.Env, Volumes: app.Volumes, MemoryMB: app.MemoryMB, CPUs: app.CPUs, GitURL: app.GitURL, GitBranch: &pr.Head,
			Dockerfile: app.Dockerfile, BuildContext: app.BuildContext, Builder: app.Builder}}
	if n.Env == nil {
		n.Env = []store.EnvVar{}
	}
	return s.store.CreateApp(r.Context(), n)
}

func (s *Server) handleListPreviews(w http.ResponseWriter, r *http.Request) {
	app, ok := s.gitApp(w, r)
	if !ok {
		return
	}
	list, err := s.store.Previews(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range list {
		withAppDisplay(&list[i])
	}
	writeJSON(w, http.StatusOK, list)
}
