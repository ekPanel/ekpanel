package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

const KindAppDeploy = "app.deploy"

// deployTimeout bounds a build; the agent stops builds after an hour too.
const deployTimeout = 70 * time.Minute

// deployLocks keeps the deployments of one app in order.
var deployLocks sync.Map

// QueueDeploy runs a deployment: build (unless it is a rollback), then release.
func (r *Runner) QueueDeploy(ctx context.Context, app store.App, d store.Deployment, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindAppDeploy, ServerID: &app.ServerID, CreatedBy: createdBy, MaxAttempts: 1,
		Params: map[string]string{"app_id": app.ID, "deployment_id": d.ID, "name": app.Name}, DedupeKey: "deploy:" + d.ID,
	})
}

// BuildImage is the tag a deployment builds.
func BuildImage(app string, d store.Deployment) string {
	return agentproto.GitImageRepo(app) + ":" + strings.ReplaceAll(d.ID, "-", "")[:12]
}

func (r *Runner) appDeploy(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		AppID        string `json:"app_id"`
		DeploymentID string `json:"deployment_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	lock, _ := deployLocks.LoadOrStore(p.AppID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	d, err := r.store.GetDeployment(ctx, p.DeploymentID)
	if err != nil {
		return nil, err
	}
	if d.Status != "queued" {
		return nil, nil // already handled
	}
	app, err := r.store.GetApp(ctx, p.AppID)
	if err != nil {
		return nil, err
	}
	logf := func(format string, args ...any) {
		r.store.AppendDeploymentLog(ctx, d.ID, fmt.Sprintf(format, args...))
	}
	fail := func(err error) (any, error) {
		logf("\n==> Failed: %v\n", err)
		if ferr := r.store.FinishDeployment(context.WithoutCancel(ctx), d.ID, "failed", err.Error()); ferr != nil {
			return nil, ferr
		}
		r.alert(ctx, app.OwnerID, "deploy_failed", "warning", map[string]string{"name": app.Name, "message": err.Error()}, "/apps/"+app.ID)
		return nil, err
	}
	if app.State == "deleting" {
		return nil, r.store.FinishDeployment(ctx, d.ID, "cancelled", "the app is being deleted")
	}

	var image string
	if d.Trigger == "rollback" && d.Image != nil {
		image = *d.Image
		if err := r.store.StartDeployment(ctx, d.ID, "releasing"); err != nil {
			return nil, err
		}
		logf("==> Returning to build %s\n", image)
	} else {
		if err := r.store.StartDeployment(ctx, d.ID, "building"); err != nil {
			return nil, err
		}
		image = BuildImage(app.Name, d)
		res, err := r.build(ctx, app, d, image)
		if err != nil {
			return fail(err)
		}
		if err := r.store.DeploymentBuilt(ctx, d.ID, res.Commit, res.Message, image); err != nil {
			return nil, err
		}
	}

	// Releasing is an ordinary apply of the app with its new image.
	logf("\n==> Releasing %s\n", image)
	previous := app.Image
	gen, err := r.store.SetAppImage(ctx, app.ID, &image)
	if err != nil {
		return fail(err)
	}
	_, rerr := r.Reconcile(ctx, app.ServerID)
	now, err := r.store.GetApp(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	if now.AppliedGeneration >= gen && now.State == "active" {
		logf("==> Live\n")
		return nil, r.store.FinishDeployment(ctx, d.ID, "live", "")
	}
	reason := rerr
	if now.Message != nil {
		reason = errors.New(*now.Message)
	} else if reason == nil {
		reason = errors.New("the server did not apply the release")
	}
	// What runs is still the previous build; say so in the panel too.
	if _, err := r.store.SetAppImage(context.WithoutCancel(ctx), app.ID, previous); err == nil {
		r.QueueReconcile(context.WithoutCancel(ctx), app.ServerID, nil)
	}
	return fail(reason)
}

// build runs the build on the app's server, keeping its output in the deployment's log.
func (r *Runner) build(ctx context.Context, app store.App, d store.Deployment, image string) (agentproto.BuildResult, error) {
	var res agentproto.BuildResult
	p := agentproto.BuildParams{App: app.Name, Image: image, Repo: *app.GitURL, Branch: d.Branch,
		Dockerfile: "Dockerfile", Context: "."}
	if d.CommitSHA != nil {
		p.Commit = *d.CommitSHA
	}
	if app.Dockerfile != nil {
		p.Dockerfile = *app.Dockerfile
	}
	if app.BuildContext != nil {
		p.Context = *app.BuildContext
	}
	if app.OwnerID != nil {
		p.Owner = *app.OwnerID
	}
	for _, e := range app.Env {
		p.Env = append(p.Env, agentproto.EnvVar{Key: e.Key, Value: e.Value})
	}
	if len(app.DeployKey) > 0 && r.secrets != nil {
		key, err := r.secrets.Open(app.DeployKey)
		if err != nil {
			return res, fmt.Errorf("open the deploy key: %w", err)
		}
		p.SSHKey = string(key)
	}

	ctx, cancel := context.WithTimeout(ctx, deployTimeout)
	defer cancel()
	stream, err := r.hub.OpenStream(ctx, app.ServerID, agentproto.OpAppBuild, func(id string) any {
		p.Stream = id
		return p
	})
	if err != nil {
		return res, err
	}
	defer stream.Close()

	// Output is written to the log at most once a second, so a chatty build does not
	// become thousands of updates.
	var buf []byte
	flush := func() {
		// A character split between two chunks waits for the rest of it.
		n := len(buf)
		for i := 1; i <= 3 && i <= len(buf); i++ {
			if utf8.RuneStart(buf[len(buf)-i]) {
				if !utf8.FullRune(buf[len(buf)-i:]) {
					n = len(buf) - i
				}
				break
			}
		}
		if n > 0 {
			r.store.AppendDeploymentLog(context.WithoutCancel(ctx), d.ID, strings.ToValidUTF8(string(buf[:n]), "�"))
			buf = append(buf[:0], buf[n:]...)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case data, ok := <-stream.Output():
			if !ok {
				flush()
				if reason := stream.Reason(); reason != "exit" {
					return res, errors.New(reason)
				}
				if err := json.Unmarshal(stream.Result(), &res); err != nil || res.Commit == "" {
					return res, errors.New("the agent did not report what it built")
				}
				return res, nil
			}
			buf = append(buf, data...)
			if len(buf) > 64<<10 {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			flush()
			return res, errors.New("the build took longer than an hour")
		}
	}
}
