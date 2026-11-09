package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

const KindPHPExtension = "php.extension"

// QueuePHPExtension installs or removes a PHP extension on a server.
func (r *Runner) QueuePHPExtension(ctx context.Context, serverID string, p agentproto.PHPExtensionParams, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindPHPExtension, ServerID: &serverID, CreatedBy: createdBy, MaxAttempts: 1, Params: p,
		DedupeKey: "php-extension:" + serverID + ":" + p.Version + ":" + p.Name,
	})
}

func (r *Runner) phpExtension(ctx context.Context, t store.Task) (any, error) {
	if t.ServerID == nil {
		return nil, errors.New("task has no server")
	}
	var p agentproto.PHPExtensionParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	_, err := r.hub.Call(ctx, *t.ServerID, agentproto.OpPHPExtension, p)
	return nil, err
}
