package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

const KindAppUpdate = "app.update"

// AppSpec is the desired state of an app as the agent receives it.
func AppSpec(a store.App) agentproto.AppSpec {
	spec := agentproto.AppSpec{
		ID: a.ID, Generation: a.Generation, Name: a.Name, Kind: a.Kind, TLS: a.TLS, Volumes: a.Volumes,
		PublicPorts: a.PublicPorts, Stopped: a.Stopped, Deleted: a.State == "deleting", DeleteData: a.DeleteData,
	}
	if a.OwnerID != nil {
		spec.Owner = *a.OwnerID
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	spec.Image, spec.Compose, spec.Service, spec.Domain = deref(a.Image), deref(a.Compose), deref(a.Service), deref(a.Domain)
	if a.ContainerPort != nil && a.HostPort != nil {
		spec.ContainerPort, spec.HostPort = *a.ContainerPort, *a.HostPort
	}
	if a.MemoryMB != nil {
		spec.MemoryMB = *a.MemoryMB
	}
	if a.CPUs != nil {
		spec.CPUs = *a.CPUs
	}
	for _, e := range a.Env {
		spec.Env = append(spec.Env, agentproto.EnvVar{Key: e.Key, Value: e.Value})
	}
	if a.Kind == "service" {
		spec.User, spec.Directory, spec.Command = deref(a.SiteUser), deref(a.Directory), deref(a.Command)
		spec.Runtime, spec.RuntimeVersion = a.Runtime, deref(a.RuntimeVersion)
	}
	return spec
}

const KindAppBuild = "app.service_build"

// QueueAppBuild runs a service app's build command, then restarts the app if it runs.
func (r *Runner) QueueAppBuild(ctx context.Context, app store.App, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindAppBuild, ServerID: &app.ServerID, CreatedBy: createdBy, MaxAttempts: 1,
		Params: map[string]string{"app_id": app.ID, "name": app.Name}, DedupeKey: "app-build:" + app.ID,
	})
}

func (r *Runner) appBuild(ctx context.Context, t store.Task) (res any, err error) {
	var p struct {
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	app, err := r.store.GetApp(ctx, p.AppID)
	if err != nil {
		return nil, err
	}
	if app.Kind != "service" || app.BuildCommand == nil || app.State == "deleting" {
		r.store.FinishAppBuild(ctx, app.ID, false, "There is nothing to build.\n")
		return nil, errors.New("the app has no build command")
	}
	ok, log := false, ""
	defer func() {
		// Whatever happened, the build is no longer running.
		if err != nil && log == "" {
			log = err.Error() + "\n"
		}
		if ferr := r.store.FinishAppBuild(context.WithoutCancel(ctx), app.ID, ok, log); ferr != nil && err == nil {
			err = ferr
		}
	}()
	callCtx, cancel := context.WithTimeout(ctx, 32*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, app.ServerID, agentproto.OpServiceBuild, agentproto.AppBuildParams{App: AppSpec(app), Command: *app.BuildCommand})
	if err != nil {
		return nil, err
	}
	var out agentproto.AppBuildResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	ok, log = out.OK, out.Log
	if !ok {
		return nil, errors.New("the build failed; see its output on the app's page")
	}
	if !app.Stopped {
		if _, err := r.hub.Call(ctx, app.ServerID, agentproto.OpAppRestart, agentproto.AppParams{Name: app.Name}); err != nil {
			return nil, fmt.Errorf("built, but the app did not restart: %w", err)
		}
	}
	return map[string]any{"built": true}, nil
}

// QueueAppUpdate pulls newer images for an app and recreates what changed.
func (r *Runner) QueueAppUpdate(ctx context.Context, app store.App, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindAppUpdate, ServerID: &app.ServerID, CreatedBy: createdBy, MaxAttempts: 1,
		Params: map[string]string{"app_id": app.ID, "name": app.Name}, DedupeKey: "app-update:" + app.ID,
	})
}

func (r *Runner) appUpdate(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	app, err := r.store.GetApp(ctx, p.AppID)
	if err != nil {
		return nil, err
	}
	if app.State == "deleting" {
		return nil, errors.New("the app is being deleted")
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Minute) // large images take a while
	defer cancel()
	_, err = r.hub.Call(callCtx, app.ServerID, agentproto.OpAppPull, agentproto.AppParams{Name: app.Name})
	return nil, err
}
