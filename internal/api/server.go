// Package api is the panel's HTTP layer: the JSON API under /api/v1, the agent endpoints
// and the embedded web UI.
package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"hostpanel/internal/hub"
	"hostpanel/internal/store"
)

type Options struct {
	// SecureCookies sets the Secure flag (and the __Host- prefix) on session cookies.
	SecureCookies bool
	// SetupToken must accompany the first-admin request while the panel has no users,
	// so whoever reaches a fresh install first cannot claim it. Empty disables setup.
	SetupToken string
	// OnSetupDone runs once after the first administrator is created.
	OnSetupDone func()
	// UI holds the built web app (index.html at the root). Nil serves a placeholder page.
	UI fs.FS
	// Version is reported by /api/v1/health and in the agent handshake.
	Version string
}

type Server struct {
	store   *store.Store
	hub     *hub.Hub
	log     *slog.Logger
	opts    Options
	limiter *rateLimiter

	setupMu    sync.Mutex
	setupToken string
}

func New(st *store.Store, h *hub.Hub, log *slog.Logger, opts Options) *Server {
	return &Server{
		store:      st,
		hub:        h,
		log:        log,
		opts:       opts,
		limiter:    newRateLimiter(10, loginWindow),
		setupToken: opts.SetupToken,
	}
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(securityHeaders)
	r.Use(s.recoverer)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(s.csrfGuard)
		r.Use(s.loadSession)

		r.Get("/health", s.handleHealth)
		r.Get("/setup/status", s.handleSetupStatus)
		r.Post("/setup", s.handleSetup)
		r.Post("/auth/login", s.handleLogin)

		// Agents authenticate with their own credentials, not browser sessions.
		r.Post("/agent/enroll", s.handleAgentEnroll)
		r.Get("/agent/connect", s.handleAgentConnect)

		r.Group(func(r chi.Router) {
			r.Use(requireUser)
			r.Post("/auth/logout", s.handleLogout)
			r.Get("/auth/me", s.handleMe)
			r.Patch("/auth/me", s.handleUpdateMe)
			r.Post("/auth/2fa/setup", s.handleTOTPSetup)
			r.Post("/auth/2fa/enable", s.handleTOTPEnable)
			r.Post("/auth/2fa/disable", s.handleTOTPDisable)
			r.Post("/auth/sessions/revoke-others", s.handleRevokeOtherSessions)
		})

		r.Group(func(r chi.Router) {
			r.Use(requireRole("admin"))
			r.Get("/servers", s.handleListServers)
			r.Post("/servers", s.handleCreateServer)
			r.Get("/servers/{id}", s.handleGetServer)
			r.Delete("/servers/{id}", s.handleDeleteServer)
			r.Post("/servers/{id}/enroll-token", s.handleRenewEnrollToken)
			r.Get("/servers/{id}/live", s.handleServerLive)
			r.Get("/audit", s.handleListAudit)
		})

		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			writeError(w, http.StatusNotFound, "not_found", "no such API endpoint")
		})
	})

	r.Handle("/*", s.uiHandler())
	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opts.Version})
}
