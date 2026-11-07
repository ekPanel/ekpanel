// Package api is the panel's HTTP layer: the JSON API under /api/v1, the agent endpoints
// and the embedded web UI.
package api

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/hub"
	"ekpanel/internal/notify"
	"ekpanel/internal/pki"
	"ekpanel/internal/secretbox"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
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
	// CA issues agent certificates; its fingerprint goes into enrolment commands.
	CA *pki.CA
	// AgentPort is the port of the agent listener, shown in enrolment commands.
	AgentPort int
	// Tasks queues background work.
	Tasks *tasks.Runner
	// Notify sends notifications; Secrets seals the secrets they need.
	Notify  *notify.Notifier
	Secrets *secretbox.Box
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

		r.Group(func(r chi.Router) {
			r.Use(requireUser)
			r.Post("/auth/logout", s.handleLogout)
			r.Get("/auth/me", s.handleMe)
			r.Patch("/auth/me", s.handleUpdateMe)
			r.Post("/auth/impersonation/end", s.handleEndImpersonation)
			r.With(notImpersonating).Post("/auth/confirm", s.handleConfirm)
			// The account's own security stays with its owner, not an administrator acting as them.
			r.With(notImpersonating).Post("/auth/2fa/setup", s.handleTOTPSetup)
			r.With(notImpersonating).Post("/auth/2fa/enable", s.handleTOTPEnable)
			r.With(notImpersonating).Post("/auth/2fa/disable", s.handleTOTPDisable)
			r.With(notImpersonating).Post("/auth/sessions/revoke-others", s.handleRevokeOtherSessions)
		})

		// Administrators and customers. Handlers show customers only what they own and
		// check their package's limits.
		r.Group(func(r chi.Router) {
			r.Use(requireRole("admin", "customer"))
			r.Get("/me/usage", s.handleMyUsage)
			r.Get("/notifications", s.handleListNotifications)
			r.Post("/notifications/read", s.handleReadNotifications)
			r.With(notImpersonating).Get("/notifications/channels", s.handleListChannels)
			r.With(notImpersonating).Post("/notifications/channels", s.handleCreateChannel)
			r.With(notImpersonating).Put("/notifications/channels/{id}", s.handleUpdateChannel)
			r.With(notImpersonating).Delete("/notifications/channels/{id}", s.handleDeleteChannel)
			r.With(notImpersonating).Post("/notifications/channels/{id}/test", s.handleTestChannel)
			r.Post("/auth/password", s.handleChangePassword)
			r.Get("/servers", s.handleListServers)
			r.Get("/tasks", s.handleListTasks)
			r.Get("/sites", s.handleListSites)
			r.Post("/sites", s.handleCreateSite)
			r.Get("/sites/{id}", s.handleGetSite)
			r.Patch("/sites/{id}", s.handleUpdateSite)
			r.Delete("/sites/{id}", s.handleDeleteSite)
			r.Post("/sites/{id}/sftp", s.handleSetSFTP)
			r.Post("/sites/{id}/sftp/password", s.handleResetSFTPPassword)
			r.Put("/sites/{id}/ssh-keys", s.handleSetSSHKeys)
			r.Post("/sites/{id}/wordpress", s.handleInstallWordPress)
			r.Post("/sites/{id}/files", s.handleFiles)
			r.Post("/sites/{id}/files/upload", s.handleUpload)
			r.Get("/sites/{id}/files/download", s.handleDownload)
			r.Get("/sites/{id}/logs", s.handleSiteLogs)
			r.Get("/sites/{id}/traffic", s.handleSiteTraffic)
			r.Get("/php-versions", s.handlePHPVersions)
			r.Get("/databases", s.handleListDatabases)
			r.Post("/databases", s.handleCreateDatabase)
			r.Get("/databases/{id}", s.handleGetDatabase)
			r.Post("/databases/{id}/password", s.handleResetDatabasePassword)
			r.Delete("/databases/{id}", s.handleDeleteDatabase)
			r.Get("/catalog", s.handleCatalog)
			r.Get("/apps", s.handleListApps)
			r.Post("/apps", s.handleCreateApp)
			r.Get("/apps/{id}", s.handleGetApp)
			r.Put("/apps/{id}", s.handleUpdateApp)
			r.Delete("/apps/{id}", s.handleDeleteApp)
			r.Post("/apps/{id}/start", s.handleStartApp)
			r.Post("/apps/{id}/stop", s.handleStopApp)
			r.Post("/apps/{id}/restart", s.handleRestartApp)
			r.Post("/apps/{id}/update", s.handleUpdateAppImages)
			r.Get("/apps/{id}/status", s.handleAppStatus)
			r.Get("/apps/{id}/logs", s.handleAppLogs)
			r.Get("/apps/{id}/access-log", s.handleAppAccessLog)
			r.Get("/apps/{id}/terminal", s.handleAppTerminal)
			r.Get("/apps/{id}/traffic", s.handleAppTraffic)
			r.Get("/cron", s.handleListCron)
			r.Post("/cron", s.handleCreateCron)
			r.Post("/cron/preview", s.handleCronPreview)
			r.Get("/cron/{id}", s.handleGetCron)
			r.Put("/cron/{id}", s.handleUpdateCron)
			r.Delete("/cron/{id}", s.handleDeleteCron)
			r.Post("/cron/{id}/run", s.handleRunCron)
			r.Get("/cron/{id}/status", s.handleCronStatus)
		})

		r.Group(func(r chi.Router) {
			r.Use(requireRole("admin"))
			r.Get("/settings/notifications", s.handleGetNotifySettings)
			r.Put("/settings/notifications", s.handleSetNotifySettings)
			r.Get("/packages", s.handleListPackages)
			r.Post("/packages", s.handleCreatePackage)
			r.Get("/packages/{id}", s.handleGetPackage)
			r.Put("/packages/{id}", s.handleUpdatePackage)
			r.Delete("/packages/{id}", s.handleDeletePackage)
			r.Get("/customers", s.handleListCustomers)
			r.Post("/customers", s.handleCreateCustomer)
			r.Get("/customers/{id}", s.handleGetCustomer)
			r.Put("/customers/{id}", s.handleUpdateCustomer)
			r.Delete("/customers/{id}", s.handleDeleteCustomer)
			r.Post("/customers/{id}/password", s.handleResetCustomerPassword)
			r.Post("/customers/{id}/impersonate", s.handleImpersonate)
			r.Post("/servers", s.handleCreateServer)
			r.Get("/servers/{id}", s.handleGetServer)
			r.Delete("/servers/{id}", s.handleDeleteServer)
			r.Post("/servers/{id}/enroll-token", s.handleRenewEnrollToken)
			r.Get("/servers/{id}/live", s.handleServerLive)
			r.Get("/servers/{id}/metrics", s.handleServerMetrics)
			r.Get("/servers/{id}/services", s.handleServerServices)
			r.Post("/servers/{id}/services/{name}/restart", s.handleRestartService)
			r.Post("/servers/{id}/usage", s.handleMeasureUsage)
			r.Get("/servers/{id}/terminal", s.handleServerTerminal)
			r.Post("/servers/{id}/traffic", s.handleCollectTraffic)
			r.Get("/servers/{id}/firewall", s.handleGetFirewall)
			r.Put("/servers/{id}/firewall", s.handleSetFirewall)
			r.Post("/servers/{id}/firewall/rules", s.handleAddFirewallRule)
			r.Delete("/servers/{id}/firewall/rules/{rule}", s.handleDeleteFirewallRule)
			r.Get("/backups", s.handleListBackupRepos)
			r.Post("/backups", s.handleCreateBackupRepo)
			r.Get("/backups/{id}", s.handleGetBackupRepo)
			r.Patch("/backups/{id}", s.handleUpdateBackupRepo)
			r.Delete("/backups/{id}", s.handleDeleteBackupRepo)
			r.Post("/backups/{id}/run", s.handleRunBackup)
			r.Get("/backups/{id}/snapshots", s.handleBackupSnapshots)
			r.Post("/backups/{id}/restore", s.handleRestoreBackup)
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
