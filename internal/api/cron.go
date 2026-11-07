package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/cron"
	"ekpanel/internal/store"
)

type cronRequest struct {
	SiteID         string `json:"site_id"`
	AppID          string `json:"app_id"`
	Service        string `json:"service"`
	Name           string `json:"name"`
	Schedule       string `json:"schedule"`
	Timezone       string `json:"timezone"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Enabled        *bool  `json:"enabled"`
}

// cronView is a job with its next run, as the UI shows it.
type cronView struct {
	store.CronJob
	NextRun *time.Time `json:"next_run"`
}

func viewCron(j store.CronJob) cronView {
	v := cronView{CronJob: j}
	if s, err := cron.Parse(j.Schedule); err == nil && j.Enabled {
		if loc, err := time.LoadLocation(j.Timezone); err == nil {
			if t := s.Next(time.Now(), loc); !t.IsZero() {
				v.NextRun = &t
			}
		}
	}
	return v
}

// cronSettings validates a job's settings. It writes the error response when they are wrong.
func cronSettings(w http.ResponseWriter, req cronRequest, forApp bool) (store.CronSettings, bool) {
	fail := func(code, msg string) (store.CronSettings, bool) {
		writeError(w, http.StatusBadRequest, code, msg)
		return store.CronSettings{}, false
	}
	set := store.CronSettings{
		Name: strings.TrimSpace(req.Name), Schedule: strings.Join(strings.Fields(req.Schedule), " "), Timezone: strings.TrimSpace(req.Timezone),
		Command: strings.TrimSpace(req.Command), TimeoutSeconds: req.TimeoutSeconds, Enabled: req.Enabled == nil || *req.Enabled,
	}
	if set.Timezone == "" {
		set.Timezone = "UTC"
	}
	if set.TimeoutSeconds == 0 {
		set.TimeoutSeconds = 3600
	}
	if set.Name == "" {
		set.Name = set.Command
		if utf8.RuneCountInString(set.Name) > 60 {
			set.Name = string([]rune(set.Name)[:60]) + "…"
		}
	}
	switch {
	case utf8.RuneCountInString(set.Name) > 100:
		return fail("invalid_name", "enter a name of up to 100 characters")
	case set.Command == "" || len(set.Command) > 4096 || strings.ContainsRune(set.Command, 0):
		return fail("invalid_command", "enter a command of up to 4096 characters")
	case set.TimeoutSeconds < 10 || set.TimeoutSeconds > 86400:
		return fail("invalid_timeout", "the time limit must be between 10 seconds and 24 hours")
	case !cron.ValidZone(set.Timezone):
		return fail("invalid_timezone", "choose a time zone such as Asia/Dhaka")
	}
	if _, err := cron.Parse(set.Schedule); err != nil {
		return fail("invalid_schedule", err.Error())
	}
	if forApp {
		service := strings.TrimSpace(req.Service)
		if service == "" {
			service = agentproto.AppServiceName
		}
		if !serviceRE.MatchString(service) {
			return fail("invalid_service", "enter the name of the app's service to run in")
		}
		set.Service = &service
	}
	return set, true
}

func (s *Server) writeCronError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "cron_not_found", "no job with that ID")
		return
	}
	s.internalError(w, r, err)
}

func (s *Server) getCron(r *http.Request) (store.CronJob, error) {
	j, err := s.store.GetCronJob(r.Context(), urlID(r))
	if err == nil && !visible(r, j.OwnerID) {
		return store.CronJob{}, store.ErrNotFound
	}
	return j, err
}

func (s *Server) handleListCron(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.CronFilter{SiteID: q.Get("site_id"), AppID: q.Get("app_id"), OwnerID: ownerScope(r)}
	if (f.SiteID != "" && !isUUID(f.SiteID)) || (f.AppID != "" && !isUUID(f.AppID)) {
		writeJSON(w, http.StatusOK, []cronView{})
		return
	}
	list, err := s.store.ListCronJobs(r.Context(), f)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]cronView, 0, len(list))
	for _, j := range list {
		out = append(out, viewCron(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateCron(w http.ResponseWriter, r *http.Request) {
	var req cronRequest
	if !readJSON(w, r, &req) {
		return
	}
	n := store.NewCronJob{}
	var ownerID *string
	switch {
	case req.SiteID != "" && req.AppID == "":
		site, err := s.getSite(r, req.SiteID)
		if err != nil || !isUUID(req.SiteID) {
			writeError(w, http.StatusNotFound, "site_not_found", "no site with that ID")
			return
		}
		if site.State == "deleting" {
			writeError(w, http.StatusConflict, "site_deleting", "this site is being deleted")
			return
		}
		n.ServerID, n.SiteID, ownerID = site.ServerID, &site.ID, site.OwnerID
	case req.AppID != "" && req.SiteID == "":
		app, err := s.getApp(r, req.AppID)
		if err != nil || !isUUID(req.AppID) {
			writeError(w, http.StatusNotFound, "app_not_found", "no app with that ID")
			return
		}
		if app.State == "deleting" {
			writeError(w, http.StatusConflict, "app_not_ready", "this app is being deleted")
			return
		}
		n.ServerID, n.AppID, ownerID = app.ServerID, &app.ID, app.OwnerID
	default:
		writeError(w, http.StatusBadRequest, "invalid_cron_target", "a job belongs to a website or an app")
		return
	}
	set, ok := cronSettings(w, req, n.AppID != nil)
	if !ok {
		return
	}
	pkg, err := s.packageFor(r, ownerID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !s.allowNew(w, r, placement{OwnerID: ownerID, Package: pkg}, "cron", n.ServerID) {
		return
	}
	u := currentUser(r)
	n.CronSettings, n.CreatedBy = set, &u.ID
	j, err := s.store.CreateCronJob(r.Context(), n)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "cron.created", j.Name, map[string]any{"cron_id": j.ID, "target": j.TargetName, "schedule": j.Schedule})
	s.queueReconcile(r, j.ServerID)
	writeJSON(w, http.StatusCreated, viewCron(j))
}

func (s *Server) handleGetCron(w http.ResponseWriter, r *http.Request) {
	j, err := s.getCron(r)
	if err != nil {
		s.writeCronError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, viewCron(j))
}

func (s *Server) handleUpdateCron(w http.ResponseWriter, r *http.Request) {
	var req cronRequest
	if !readJSON(w, r, &req) {
		return
	}
	j, err := s.getCron(r)
	if err != nil {
		s.writeCronError(w, r, err)
		return
	}
	set, ok := cronSettings(w, req, j.AppID != nil)
	if !ok {
		return
	}
	j, err = s.store.UpdateCronJob(r.Context(), j.ID, set)
	if err != nil {
		s.writeCronError(w, r, err)
		return
	}
	s.audit(r, "cron.updated", j.Name, map[string]any{"cron_id": j.ID, "schedule": j.Schedule, "enabled": j.Enabled})
	s.queueReconcile(r, j.ServerID)
	writeJSON(w, http.StatusOK, viewCron(j))
}

func (s *Server) handleDeleteCron(w http.ResponseWriter, r *http.Request) {
	j, err := s.getCron(r)
	if err == nil {
		j, err = s.store.MarkCronDeleting(r.Context(), j.ID)
	}
	if err != nil {
		s.writeCronError(w, r, err)
		return
	}
	s.audit(r, "cron.deleted", j.Name, map[string]any{"cron_id": j.ID})
	s.queueReconcile(r, j.ServerID)
	writeJSON(w, http.StatusAccepted, viewCron(j))
}

// cronCall runs an operation on the job's server once the job exists there.
func (s *Server) cronCall(w http.ResponseWriter, r *http.Request, op string) (store.CronJob, []byte, bool) {
	j, err := s.getCron(r)
	if err != nil {
		s.writeCronError(w, r, err)
		return j, nil, false
	}
	if j.AppliedGeneration == 0 {
		writeError(w, http.StatusConflict, "cron_not_ready", "the job has not been set up on its server yet")
		return j, nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, j.ServerID, op, agentproto.CronParams{ID: j.ID})
	if err != nil {
		s.writeAgentError(w, r, err)
		return j, nil, false
	}
	return j, raw, true
}

func (s *Server) handleRunCron(w http.ResponseWriter, r *http.Request) {
	j, _, ok := s.cronCall(w, r, agentproto.OpCronRun)
	if !ok {
		return
	}
	s.audit(r, "cron.run", j.Name, map[string]any{"cron_id": j.ID})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCronStatus(w http.ResponseWriter, r *http.Request) {
	if _, raw, ok := s.cronCall(w, r, agentproto.OpCronStatus); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(raw)
	}
}

// handleCronPreview checks a schedule and lists its next runs, for the editor.
func (s *Server) handleCronPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Schedule string `json:"schedule"`
		Timezone string `json:"timezone"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if !cron.ValidZone(req.Timezone) {
		writeError(w, http.StatusBadRequest, "invalid_timezone", "choose a time zone such as Asia/Dhaka")
		return
	}
	sched, err := cron.Parse(req.Schedule)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	loc, _ := time.LoadLocation(req.Timezone)
	next := []time.Time{}
	t := time.Now()
	for range 5 {
		t = sched.Next(t, loc)
		if t.IsZero() {
			break
		}
		next = append(next, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": next})
}
