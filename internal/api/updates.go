package api

import (
	"net/http"
	"slices"
	"strings"

	"ekpanel/internal/release"
	"ekpanel/internal/tasks"
	"ekpanel/internal/updates"
)

// Updates of the panel and its agents (Settings → Updates). Administrators only.

type agentVersion struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	AgentVersion string `json:"agent_version"`
	Online       bool   `json:"online"`
	PanelHost    bool   `json:"panel_host"`
	Outdated     bool   `json:"outdated"`
}

type updatesView struct {
	Current string `json:"current"`
	// Release is false for a panel built from source ("dev"): it cannot update itself.
	Release         bool           `json:"release"`
	Latest          updates.Latest `json:"latest"`
	UpdateAvailable bool           `json:"update_available"`
	CheckEnabled    bool           `json:"check_enabled"`
	// CanUpdate: the agent on the panel's own server is connected and can install updates.
	CanUpdate bool               `json:"can_update"`
	State     *tasks.UpdateState `json:"state"`
	Servers   []agentVersion     `json:"servers"`
	Outdated  int                `json:"outdated"`
}

func (s *Server) updatesView(r *http.Request, latest updates.Latest) (updatesView, error) {
	ctx := r.Context()
	cur := s.opts.Version
	v := updatesView{
		Current: cur, Release: release.Valid(cur), Latest: latest,
		UpdateAvailable: release.Valid(cur) && release.Compare(latest.Version, cur) > 0,
		CheckEnabled:    s.opts.Updates != nil,
		Servers:         []agentVersion{},
	}
	st, err := s.opts.Tasks.UpdateState(ctx)
	if err != nil {
		return v, err
	}
	if st.Phase != "" {
		v.State = &st
	}
	servers, err := s.store.ListServers(ctx)
	if err != nil {
		return v, err
	}
	for _, srv := range servers {
		if srv.EnrolledAt == nil {
			continue
		}
		online, _ := s.hub.Latest(srv.ID)
		a := agentVersion{ID: srv.ID, Name: srv.Name, AgentVersion: srv.AgentVersion, Online: online, PanelHost: srv.PanelHost,
			Outdated: v.Release && release.Compare(srv.AgentVersion, cur) < 0}
		if a.Outdated {
			v.Outdated++
		}
		if srv.PanelHost && online {
			v.CanUpdate = true
		}
		v.Servers = append(v.Servers, a)
	}
	slices.SortStableFunc(v.Servers, func(a, b agentVersion) int {
		if a.PanelHost != b.PanelHost {
			if a.PanelHost {
				return -1
			}
			return 1
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return v, nil
}

func (s *Server) latestRelease(r *http.Request) (updates.Latest, error) {
	if s.opts.Updates == nil {
		return updates.Latest{}, nil
	}
	return s.opts.Updates.Get(r.Context())
}

func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	latest, err := s.latestRelease(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v, err := s.updatesView(r, latest)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleCheckUpdates(w http.ResponseWriter, r *http.Request) {
	if s.opts.Updates == nil {
		writeError(w, http.StatusConflict, "updates_off", "update checks are turned off on this panel")
		return
	}
	latest, err := s.opts.Updates.Check(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	v, err := s.updatesView(r, latest)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// updateBlocked writes an error and returns true when no update may start now.
func (s *Server) updateBlocked(w http.ResponseWriter, r *http.Request) bool {
	st, err := s.opts.Tasks.UpdateState(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return true
	}
	if st.Running() {
		writeError(w, http.StatusConflict, "update_running", "an update is already running")
		return true
	}
	return false
}

func (s *Server) handleUpdatePanel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	// A panel built from source is not installed from a release: there is nothing to swap
	// it for safely, and no previous release to go back to.
	if !release.Valid(s.opts.Version) {
		writeError(w, http.StatusConflict, "not_a_release", "this panel is built from source, so it does not update itself; install a release")
		return
	}
	if s.updateBlocked(w, r) {
		return
	}
	latest, err := s.latestRelease(r)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Only the release the panel found, and only forwards.
	if req.Version == "" || req.Version != latest.Version || release.Compare(req.Version, s.opts.Version) <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_version", "update only to the newest release, as found by Check for updates")
		return
	}
	if _, online, err := s.opts.Tasks.PanelHost(r.Context()); err != nil {
		s.internalError(w, r, err)
		return
	} else if !online {
		writeError(w, http.StatusConflict, "panel_agent_offline", "the agent on the panel's own server is not connected")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueUpdatePanel(r.Context(), req.Version, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "system.update", req.Version, map[string]any{"from": s.opts.Version, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleUpdateAgents(w http.ResponseWriter, r *http.Request) {
	if !release.Valid(s.opts.Version) {
		writeError(w, http.StatusConflict, "not_a_release", "this panel is built from source, so there is no release to update the agents to")
		return
	}
	if s.updateBlocked(w, r) {
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueUpdateAgents(r.Context(), &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "system.update_agents", s.opts.Version, map[string]any{"task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
