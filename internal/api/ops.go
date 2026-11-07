package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/hub"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// writeAgentError maps a failed agent call to a response the UI can translate.
func (s *Server) writeAgentError(w http.ResponseWriter, r *http.Request, err error) {
	var opErr *agentproto.OpError
	switch {
	case errors.Is(err, hub.ErrOffline), errors.Is(err, errAgentGone):
		writeError(w, http.StatusConflict, "agent_offline", "the server's agent is not connected")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "agent_timeout", "the agent did not answer in time")
	case errors.As(err, &opErr):
		writeError(w, http.StatusBadGateway, "agent_"+opErr.Code, opErr.Message)
	default:
		s.internalError(w, r, err)
	}
}

// handleServerServices asks the agent for the state of the services the panel watches.
func (s *Server) handleServerServices(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, id, agentproto.OpServicesStatus, struct{}{})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	var res agentproto.ServicesStatusResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

var serviceName = regexp.MustCompile(`^[a-z0-9][a-z0-9@._-]{0,63}$`)

// handleRestartService queues a restart. The agent decides which services may be restarted.
func (s *Server) handleRestartService(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	srv, err := s.store.GetServer(r.Context(), id)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	name := chi.URLParam(r, "name")
	if !serviceName.MatchString(name) {
		writeError(w, http.StatusBadRequest, "invalid_service", "that is not a service name")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.Enqueue(r.Context(), store.NewTask{
		Kind: tasks.KindServiceRestart, ServerID: &srv.ID, CreatedBy: &u.ID,
		Params: agentproto.ServicesRestartParams{Name: name},
	})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "service.restart_requested", srv.Name+" / "+name, map[string]any{"server_id": srv.ID, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskFilter{ActiveOnly: q.Get("active") == "1"}
	if sid := q.Get("server_id"); sid != "" {
		if !isUUID(sid) {
			writeJSON(w, http.StatusOK, []store.Task{})
			return
		}
		f.ServerID = sid
	}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	list, err := s.store.ListTasks(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// metricRanges maps the UI's range choices to how history is read. Steps keep every
// chart at a few hundred points.
var metricRanges = map[string]struct {
	span, step time.Duration
	resolution int
}{
	"1h":  {time.Hour, time.Minute, 60},
	"24h": {24 * time.Hour, 5 * time.Minute, 60},
	"7d":  {7 * 24 * time.Hour, time.Hour, 3600},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour, 3600},
}

func (s *Server) handleServerMetrics(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	rg, ok := metricRanges[r.URL.Query().Get("range")]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_range", "range must be 1h, 24h, 7d or 30d")
		return
	}
	points, err := s.store.MetricRange(r.Context(), id, rg.span, rg.step, rg.resolution)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"step_seconds": int(rg.step.Seconds()), "points": points})
}
