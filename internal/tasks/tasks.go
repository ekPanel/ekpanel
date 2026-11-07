// Package tasks runs background work stored in the tasks table: anything slow or anything
// that asks an agent to change a server. Users see each task's status in the panel.
package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/hub"
	"ekpanel/internal/store"
)

// Kinds of task. Each has a handler registered in New.
const (
	KindServiceRestart = "service.restart"
)

// lease is how long a worker may hold a task before it is considered crashed.
// Handlers must finish (or time out) well within it.
const lease = 10 * time.Minute

type Handler func(ctx context.Context, t store.Task) (result any, err error)

type Runner struct {
	store    *store.Store
	hub      *hub.Hub
	log      *slog.Logger
	handlers map[string]Handler
	wake     chan struct{}
}

func New(st *store.Store, h *hub.Hub, log *slog.Logger) *Runner {
	r := &Runner{store: st, hub: h, log: log, wake: make(chan struct{}, 1), handlers: map[string]Handler{}}
	r.handlers[KindServiceRestart] = r.serviceRestart
	return r
}

// Enqueue stores a task and wakes a worker. Kind must have a handler.
func (r *Runner) Enqueue(ctx context.Context, n store.NewTask) (store.Task, error) {
	if r.handlers[n.Kind] == nil {
		return store.Task{}, fmt.Errorf("no handler for task kind %q", n.Kind)
	}
	t, err := r.store.CreateTask(ctx, n)
	if err == nil {
		select {
		case r.wake <- struct{}{}:
		default:
		}
	}
	return t, err
}

// Run starts the workers and housekeeping, and blocks until ctx ends and every running
// task has returned.
func (r *Runner) Run(ctx context.Context, workers int) {
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work(ctx)
		}()
	}
	r.housekeeping(ctx)
	wg.Wait()
}

func (r *Runner) work(ctx context.Context) {
	idle := time.NewTicker(time.Second)
	defer idle.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		t, err := r.store.ClaimTask(ctx, lease)
		if errors.Is(err, store.ErrNotFound) {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			case <-idle.C:
			}
			continue
		}
		if err != nil {
			if ctx.Err() == nil {
				r.log.Error("claim task", "err", err)
				time.Sleep(time.Second)
			}
			continue
		}
		r.execute(ctx, t)
	}
}

func (r *Runner) execute(ctx context.Context, t store.Task) {
	// A task in progress gets to finish its bookkeeping even if the panel is stopping.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lease-time.Minute)
	defer cancel()
	log := r.log.With("task", t.ID, "kind", t.Kind)

	result, err := func() (res any, err error) {
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("task crashed: %v", v)
			}
		}()
		handler := r.handlers[t.Kind]
		if handler == nil {
			return nil, fmt.Errorf("this panel version cannot run %q tasks", t.Kind)
		}
		return handler(runCtx, t)
	}()
	if err != nil {
		log.Warn("task failed", "attempt", t.Attempts, "err", err)
		backoff := time.Duration(t.Attempts*t.Attempts) * 10 * time.Second
		if err := r.store.FailTask(runCtx, t.ID, err.Error(), backoff); err != nil {
			log.Error("record task failure", "err", err)
		}
		return
	}
	if err := r.store.FinishTask(runCtx, t.ID, result); err != nil {
		log.Error("record task result", "err", err)
	}
}

func (r *Runner) housekeeping(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		if n, err := r.store.RecoverTasks(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("recover tasks", "err", err)
		} else if n > 0 {
			r.log.Warn("recovered interrupted tasks", "count", n)
		}
		if err := r.store.DeleteOldTasks(ctx, 30*24*time.Hour); err != nil && ctx.Err() == nil {
			r.log.Error("delete old tasks", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---- Handlers ----

func (r *Runner) serviceRestart(ctx context.Context, t store.Task) (any, error) {
	if t.ServerID == nil {
		return nil, errors.New("task has no server")
	}
	var p agentproto.ServicesRestartParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(ctx, *t.ServerID, agentproto.OpServicesRestart, p)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
