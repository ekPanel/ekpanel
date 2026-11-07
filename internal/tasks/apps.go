package tasks

import (
	"context"
	"encoding/json"
	"errors"
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
	return spec
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
