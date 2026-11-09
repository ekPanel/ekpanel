package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/release"
	"ekpanel/internal/store"
)

// Self-update, in two tasks. KindUpdatePanel asks the agent on the panel's own server to
// install a new panel; that agent restarts the panel. The new panel finds the update in
// progress when it starts (ResumeUpdate) and queues KindUpdateAgents, which updates the
// agents one server at a time to the panel's own version.
const (
	KindUpdatePanel  = "system.update_panel"
	KindUpdateAgents = "system.update_agents"
)

// updateStateKey is the settings key of the latest update's progress.
const updateStateKey = "update.state"

// How long an agent has to come back with the new version after it restarts. Its rollback
// timer (3 minutes) fires before this ends, so a broken update shows as the old version.
const agentReturnWait = 4 * time.Minute

// Update phases.
const (
	UpdatePanel  = "panel"
	UpdateAgents = "agents"
	UpdateDone   = "done"
	UpdateFailed = "failed"
)

type UpdateState struct {
	Target     string         `json:"target"`
	From       string         `json:"from,omitempty"`
	Phase      string         `json:"phase"`
	Error      string         `json:"error,omitempty"`
	Backup     string         `json:"backup,omitempty"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt *time.Time     `json:"finished_at,omitempty"`
	Servers    []ServerUpdate `json:"servers,omitempty"`
}

// ServerUpdate is what happened to one server's agent.
type ServerUpdate struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	From     string `json:"from"`
	// Status: updated, current (already at the version), offline, failed.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// SetRelease tells the runner which version the panel is and where releases come from.
func (r *Runner) SetRelease(version, releasesURL string) {
	r.version, r.releasesURL = version, releasesURL
}

// SetUpdateWait changes how long an updated agent has to come back (tests).
func (r *Runner) SetUpdateWait(d time.Duration) { r.agentWait = d }

// errStopping ends an update that the panel's own shutdown interrupts; it goes on when
// the panel starts again.
var errStopping = errors.New("the panel is stopping")

// Version is the panel's own version.
func (r *Runner) Version() string { return r.version }

// UpdateState returns the latest update's progress (Phase is empty when there was none).
func (r *Runner) UpdateState(ctx context.Context) (UpdateState, error) {
	var st UpdateState
	_, err := r.store.GetSetting(ctx, updateStateKey, &st)
	return st, err
}

func (r *Runner) saveUpdate(ctx context.Context, st UpdateState) {
	if err := r.store.SetSetting(ctx, updateStateKey, st); err != nil {
		r.log.Error("save update progress", "err", err)
	}
}

// Running reports whether an update is in progress.
func (st UpdateState) Running() bool { return st.Phase == UpdatePanel || st.Phase == UpdateAgents }

// QueueUpdatePanel starts an update of the panel to version.
func (r *Runner) QueueUpdatePanel(ctx context.Context, version string, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{
		Kind: KindUpdatePanel, Params: map[string]string{"version": version}, CreatedBy: createdBy,
		MaxAttempts: 1, DedupeKey: "update",
	})
}

// QueueUpdateAgents updates every agent to the panel's version.
func (r *Runner) QueueUpdateAgents(ctx context.Context, createdBy *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindUpdateAgents, CreatedBy: createdBy, MaxAttempts: 1, DedupeKey: "update"})
}

// PanelHost is the online server running the panel, whose agent can update it.
func (r *Runner) PanelHost(ctx context.Context) (store.Server, bool, error) {
	servers, err := r.store.ListServers(ctx)
	if err != nil {
		return store.Server{}, false, err
	}
	for _, s := range servers {
		if s.PanelHost {
			online, _ := r.hub.Latest(s.ID)
			return s, online, nil
		}
	}
	return store.Server{}, false, nil
}

func (r *Runner) failUpdate(ctx context.Context, st UpdateState, err error) error {
	now := time.Now()
	st.Phase, st.Error, st.FinishedAt = UpdateFailed, err.Error(), &now
	r.saveUpdate(ctx, st)
	r.alert(ctx, nil, "update_failed", "critical", map[string]string{"version": st.Target, "message": err.Error()}, "/settings/updates")
	return err
}

func (r *Runner) updatePanel(ctx context.Context, t store.Task) (any, error) {
	var p struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	st := UpdateState{Target: p.Version, From: r.version, Phase: UpdatePanel, StartedAt: time.Now()}
	if release.Compare(p.Version, r.version) <= 0 {
		return nil, r.failUpdate(ctx, st, fmt.Errorf("the panel is already at %s", r.version))
	}
	srv, online, err := r.PanelHost(ctx)
	if err != nil {
		return nil, err
	}
	if srv.ID == "" || !online {
		return nil, r.failUpdate(ctx, st, errors.New("the agent on the panel's own server is not connected, so it cannot install the update"))
	}
	r.saveUpdate(ctx, st)

	callCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, srv.ID, agentproto.OpSystemUpdate, agentproto.SystemUpdateParams{
		Component: agentproto.ComponentPanel, Version: p.Version, ReleasesURL: r.releasesURL,
	})
	if err != nil {
		return nil, r.failUpdate(ctx, st, err)
	}
	var res agentproto.SystemUpdateResult
	json.Unmarshal(raw, &res)
	st.Backup = res.Backup
	r.saveUpdate(ctx, st)
	// The agent restarts the panel in a moment; the new panel continues in ResumeUpdate.
	return res, nil
}

// ResumeUpdate runs when the panel starts: if it was started by an update of itself, it
// goes on with the agents, or records that the new version did not come up. An agents
// phase cut off by a restart goes on too.
func (r *Runner) ResumeUpdate(ctx context.Context) {
	st, err := r.UpdateState(ctx)
	if err != nil {
		return
	}
	if st.Phase == UpdateAgents {
		if _, err := r.Enqueue(ctx, store.NewTask{Kind: KindUpdateAgents, MaxAttempts: 1, DedupeKey: "update"}); err != nil {
			r.failUpdate(ctx, st, err)
		}
		return
	}
	if st.Phase != UpdatePanel {
		return
	}
	if r.version != st.Target {
		// The agent put the previous panel back, or the panel restarted before the update.
		r.failUpdate(ctx, st, fmt.Errorf("the panel is still at %s: version %s did not start, so the previous one was kept", r.version, st.Target))
		return
	}
	r.log.Info("the panel was updated; updating the agents next", "from", st.From, "to", st.Target)
	st.Phase = UpdateAgents
	r.saveUpdate(ctx, st)
	if _, err := r.Enqueue(ctx, store.NewTask{Kind: KindUpdateAgents, MaxAttempts: 1, DedupeKey: "update"}); err != nil {
		r.failUpdate(ctx, st, err)
	}
}

func (r *Runner) updateAgents(ctx context.Context, t store.Task) (any, error) {
	target := r.version
	st, _ := r.UpdateState(ctx)
	if st.Target != target || st.Phase != UpdateAgents {
		st = UpdateState{Target: target, Phase: UpdateAgents, StartedAt: time.Now()}
	}
	st.Servers = nil
	if !release.Valid(target) {
		return nil, r.failUpdate(ctx, st, fmt.Errorf("this panel (%s) is not a release, so there is no release to update the agents to", target))
	}
	r.saveUpdate(ctx, st)

	servers, err := r.store.ListServers(ctx)
	if err != nil {
		return nil, r.failUpdate(ctx, st, err)
	}
	// The panel's own server first: it is the one most likely to be watched.
	slices.SortStableFunc(servers, func(a, b store.Server) int {
		switch {
		case a.PanelHost == b.PanelHost:
			return 0
		case a.PanelHost:
			return -1
		}
		return 1
	})
	problems := 0
	for _, s := range servers {
		if s.EnrolledAt == nil {
			continue
		}
		su := ServerUpdate{ServerID: s.ID, Name: s.Name, From: s.AgentVersion}
		online, _ := r.hub.Latest(s.ID)
		if !online && s.LastSeenAt != nil && time.Since(*s.LastSeenAt) < 5*time.Minute {
			// Just after the panel's own update, agents are still reconnecting.
			online = r.waitOnline(ctx, s.ID, min(time.Minute, r.agentWait))
		}
		switch {
		case release.Compare(s.AgentVersion, target) >= 0:
			su.Status = "current"
		case !online:
			su.Status = "offline"
			problems++
		default:
			if err := r.updateAgent(ctx, s.ID, target); errors.Is(err, errStopping) {
				return nil, err // the next start goes on (ResumeUpdate)
			} else if err != nil {
				su.Status, su.Error = "failed", err.Error()
				problems++
			} else {
				su.Status = "updated"
			}
		}
		st.Servers = append(st.Servers, su)
		r.saveUpdate(ctx, st)
	}

	now := time.Now()
	st.Phase, st.FinishedAt = UpdateDone, &now
	r.saveUpdate(ctx, st)
	severity := "info"
	if problems > 0 {
		severity = "warning"
	}
	r.alert(ctx, nil, "update_finished", severity, map[string]string{
		"version": target, "servers": fmt.Sprint(len(st.Servers)), "problems": fmt.Sprint(problems),
	}, "/settings/updates")
	return st, nil
}

// waitOnline waits up to d for a server's agent to be connected.
func (r *Runner) waitOnline(ctx context.Context, serverID string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if online, _ := r.hub.Latest(serverID); online {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-r.stopping:
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// updateAgent installs target on one server and waits for its agent to come back with it.
func (r *Runner) updateAgent(ctx context.Context, serverID, target string) error {
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if _, err := r.hub.Call(callCtx, serverID, agentproto.OpSystemUpdate, agentproto.SystemUpdateParams{
		Component: agentproto.ComponentAgent, Version: target, ReleasesURL: r.releasesURL,
	}); err != nil {
		return err
	}
	deadline := time.Now().Add(r.agentWait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.stopping:
			return errStopping
		case <-time.After(min(2*time.Second, r.agentWait/4)):
		}
		s, err := r.store.GetServer(ctx, serverID)
		if err != nil {
			return err
		}
		if online, _ := r.hub.Latest(serverID); online && s.AgentVersion == target {
			return nil
		}
	}
	return fmt.Errorf("the agent did not come back as %s within %s; it puts the previous version back by itself", target, r.agentWait)
}
