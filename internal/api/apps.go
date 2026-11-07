package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

var (
	appNameRE    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	imageRefRE   = regexp.MustCompile(`^[a-z0-9][a-zA-Z0-9._/:@-]{0,254}$`)
	serviceRE    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	envKeyRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	volumePathRE = regexp.MustCompile(`^/[A-Za-z0-9._/-]{0,250}$`)
)

const (
	maxComposeBytes = 256 << 10
	maxEnvVars      = 200
	maxVolumes      = 20
)

type appRequest struct {
	ServerID      string         `json:"server_id"`
	Name          string         `json:"name"`
	Kind          string         `json:"kind"`
	Image         string         `json:"image"`
	Compose       string         `json:"compose"`
	Service       string         `json:"service"`
	ContainerPort *int           `json:"container_port"`
	Domain        string         `json:"domain"`
	TLS           string         `json:"tls"`
	Env           []store.EnvVar `json:"env"`
	Volumes       []string       `json:"volumes"`
	MemoryMB      *int           `json:"memory_mb"`
	CPUs          *float64       `json:"cpus"`
	PublicPorts   bool           `json:"public_ports"`
	OwnerID       string         `json:"owner_id"` // administrators: create the app for this customer
}

// customerAppRules applies a package to an app: customers run only images the package
// allows, never Compose files (which can do anything Docker can, as root).
func customerAppRules(w http.ResponseWriter, pkg *store.Package, kind string, set store.AppSettings) bool {
	switch {
	case pkg == nil:
		return true
	case kind != "image":
		writeError(w, http.StatusForbidden, "compose_not_allowed", "customers can run images, not Compose files")
	case !imageAllowed(pkg, *set.Image):
		writeError(w, http.StatusForbidden, "image_not_allowed", "the package does not include this image")
	default:
		return true
	}
	return false
}

// appSettings validates everything that can change after an app is created, for an app of
// the given kind. It writes the error response and returns false on failure.
func appSettings(w http.ResponseWriter, kind string, req appRequest) (store.AppSettings, bool) {
	fail := func(code, msg string) (store.AppSettings, bool) {
		writeError(w, http.StatusBadRequest, code, msg)
		return store.AppSettings{}, false
	}
	set := store.AppSettings{Env: []store.EnvVar{}, Volumes: []string{}, TLS: req.TLS, PublicPorts: req.PublicPorts}
	switch kind {
	case "image":
		image := strings.TrimSpace(req.Image)
		if !imageRefRE.MatchString(image) || strings.Contains(image, "..") {
			return fail("invalid_image", "enter an image such as nginx:1.27 or ghcr.io/owner/app:latest")
		}
		set.Image = &image
		set.PublicPorts = false
		for _, v := range req.Volumes {
			v = strings.TrimRight(strings.TrimSpace(v), "/")
			if v == "" {
				continue
			}
			if !volumePathRE.MatchString(v) || strings.Contains(v, "..") {
				return fail("invalid_volume", "a kept folder must be a path inside the container, such as /data")
			}
			if !slices.Contains(set.Volumes, v) {
				set.Volumes = append(set.Volumes, v)
			}
		}
		if len(set.Volumes) > maxVolumes {
			return fail("invalid_volume", "keep at most 20 folders")
		}
	case "compose":
		if strings.TrimSpace(req.Compose) == "" || len(req.Compose) > maxComposeBytes {
			return fail("invalid_compose", "paste a Compose file of up to 256 KB")
		}
		compose := req.Compose
		set.Compose = &compose
	default:
		return fail("invalid_app_kind", "an app runs an image or a Compose file")
	}

	if req.ContainerPort != nil {
		if *req.ContainerPort < 1 || *req.ContainerPort > 65535 {
			return fail("invalid_port", "the container port must be between 1 and 65535")
		}
		service := agentproto.AppServiceName
		if kind == "compose" {
			service = strings.TrimSpace(req.Service)
			if !serviceRE.MatchString(service) {
				return fail("invalid_service", "enter the name of the service in the Compose file that listens on this port")
			}
		}
		set.ContainerPort, set.Service = req.ContainerPort, &service
	}
	if strings.TrimSpace(req.Domain) != "" {
		domain := normalizeHostname(req.Domain)
		if domain == "" {
			return fail("invalid_domain", "enter a domain name such as app.example.com")
		}
		if set.ContainerPort == nil {
			return fail("domain_needs_port", "a domain needs the port the app listens on")
		}
		set.Domain = &domain
	}
	if set.TLS == "" {
		set.TLS = "auto"
	}
	if set.TLS != "auto" && set.TLS != "internal" {
		return fail("invalid_tls", "SSL must be automatic or self-signed")
	}

	if len(req.Env) > maxEnvVars {
		return fail("invalid_env", "an app can have at most 200 variables")
	}
	seen := map[string]bool{}
	for _, e := range req.Env {
		e.Key = strings.TrimSpace(e.Key)
		if e.Key == "" && e.Value == "" {
			continue
		}
		if !envKeyRE.MatchString(e.Key) || seen[e.Key] || strings.ContainsRune(e.Value, 0) || len(e.Value) > 32<<10 {
			return fail("invalid_env", "variable names use letters, digits and _, do not start with a digit, and appear once: "+e.Key)
		}
		seen[e.Key] = true
		set.Env = append(set.Env, e)
	}

	if req.MemoryMB != nil && (*req.MemoryMB < 16 || *req.MemoryMB > 1048576) {
		return fail("invalid_limits", "the memory limit must be at least 16 MB")
	}
	if req.CPUs != nil && (*req.CPUs <= 0 || *req.CPUs > 1024) {
		return fail("invalid_limits", "the CPU limit must be more than 0")
	}
	set.MemoryMB, set.CPUs = req.MemoryMB, req.CPUs
	return set, true
}

func withAppDisplay(apps ...*store.App) {
	for _, a := range apps {
		if a.Domain != nil {
			a.DomainDisplay = toDisplay(*a.Domain)
		}
	}
}

func appID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) writeAppStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var taken store.ErrHostnameTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, "hostname_taken", taken.Error())
	case errors.Is(err, store.ErrAppNameTaken):
		writeError(w, http.StatusConflict, "app_name_taken", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "app_not_found", "no app with that ID")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	serverFilter := r.URL.Query().Get("server_id")
	if serverFilter != "" && !isUUID(serverFilter) {
		writeJSON(w, http.StatusOK, []store.App{})
		return
	}
	list, err := s.store.ListAppsOf(r.Context(), serverFilter, ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range list {
		withAppDisplay(&list[i])
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var req appRequest
	if !readJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !appNameRE.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "invalid_app_name", "use 2–32 lowercase letters, digits and dashes, starting with a letter")
		return
	}
	set, ok := appSettings(w, req.Kind, req)
	if !ok {
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	srv, err := s.store.GetServer(r.Context(), req.ServerID)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	place, ok := s.resolveOwner(w, r, req.OwnerID)
	if !ok || !customerAppRules(w, place.Package, req.Kind, set) || !s.allowNew(w, r, place, "app", srv.ID) {
		return
	}
	u := currentUser(r)
	app, err := s.store.CreateApp(r.Context(), store.NewApp{ServerID: srv.ID, Name: req.Name, Kind: req.Kind, OwnerID: place.OwnerID,
		CreatedBy: &u.ID, AppSettings: set})
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	s.audit(r, "app.created", app.Name, map[string]any{"app_id": app.ID, "server_id": srv.ID, "kind": app.Kind})
	s.queueReconcile(r, srv.ID)
	withAppDisplay(&app)
	writeJSON(w, http.StatusCreated, app)
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	withAppDisplay(&app)
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	var req appRequest
	if !readJSON(w, r, &req) {
		return
	}
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	set, ok := appSettings(w, app.Kind, req)
	if !ok {
		return
	}
	pkg, err := s.packageFor(r, app.OwnerID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !customerAppRules(w, pkg, app.Kind, set) {
		return
	}
	app, err = s.store.UpdateApp(r.Context(), app.ID, set)
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	s.audit(r, "app.updated", app.Name, map[string]any{"app_id": app.ID})
	s.queueReconcile(r, app.ServerID)
	withAppDisplay(&app)
	writeJSON(w, http.StatusOK, app)
}

func (s *Server) setAppStopped(stopped bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := s.getApp(r, appID(r))
		if err == nil {
			app, err = s.store.SetAppStopped(r.Context(), app.ID, stopped)
		}
		if err != nil {
			s.writeAppStoreError(w, r, err)
			return
		}
		action := "app.started"
		if stopped {
			action = "app.stopped"
		}
		s.audit(r, action, app.Name, map[string]any{"app_id": app.ID})
		s.queueReconcile(r, app.ServerID)
		withAppDisplay(&app)
		writeJSON(w, http.StatusOK, app)
	}
}

// handleDeleteApp removes the app's containers. Its volumes and folder are deleted too
// only with ?data=delete.
func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	deleteData := r.URL.Query().Get("data") == "delete"
	app, err = s.store.MarkAppDeleting(r.Context(), app.ID, deleteData)
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	s.audit(r, "app.deleted", app.Name, map[string]any{"app_id": app.ID, "delete_data": deleteData})
	s.queueReconcile(r, app.ServerID)
	writeJSON(w, http.StatusAccepted, app)
}

// appCall runs one operation on an app's agent and passes its JSON answer through.
func (s *Server) appCall(op string, timeout time.Duration, params func(*http.Request, store.App) agentproto.AppParams) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, err := s.getApp(r, appID(r))
		if err != nil {
			s.writeAppStoreError(w, r, err)
			return
		}
		if app.AppliedGeneration == 0 {
			writeError(w, http.StatusConflict, "app_not_ready", "the app has not been set up on its server yet")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		raw, err := s.hub.Call(ctx, app.ServerID, op, params(r, app))
		if err != nil {
			s.writeAgentError(w, r, err)
			return
		}
		if op == agentproto.OpAppRestart {
			s.audit(r, "app.restarted", app.Name, map[string]any{"app_id": app.ID})
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write(raw)
	}
}

func (s *Server) handleStartApp(w http.ResponseWriter, r *http.Request) { s.setAppStopped(false)(w, r) }
func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request)  { s.setAppStopped(true)(w, r) }

func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	s.appCall(agentproto.OpAppRestart, 2*time.Minute, appNameParams)(w, r)
}

func (s *Server) handleAppStatus(w http.ResponseWriter, r *http.Request) {
	s.appCall(agentproto.OpAppStatus, 30*time.Second, appNameParams)(w, r)
}

func (s *Server) handleAppLogs(w http.ResponseWriter, r *http.Request) {
	s.appCall(agentproto.OpAppLogs, 30*time.Second, appLogParams)(w, r)
}

func appNameParams(_ *http.Request, a store.App) agentproto.AppParams {
	return agentproto.AppParams{Name: a.Name}
}

func appLogParams(r *http.Request, a store.App) agentproto.AppParams {
	tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
	return agentproto.AppParams{Name: a.Name, Service: r.URL.Query().Get("service"), Tail: tail}
}

func (s *Server) handleUpdateAppImages(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	if app.AppliedGeneration == 0 {
		writeError(w, http.StatusConflict, "app_not_ready", "the app has not been set up on its server yet")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueAppUpdate(r.Context(), app, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "app.update_started", app.Name, map[string]any{"app_id": app.ID, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
