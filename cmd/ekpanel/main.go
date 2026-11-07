// Command ekpanel runs the control panel: web UI, API and the endpoint agents connect to.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ekpanel/internal/api"
	"ekpanel/internal/auth"
	"ekpanel/internal/config"
	"ekpanel/internal/db"
	"ekpanel/internal/hub"
	"ekpanel/internal/notify"
	"ekpanel/internal/pki"
	"ekpanel/internal/secretbox"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
	"ekpanel/internal/webui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "version":
		fmt.Println(version)
	case len(os.Args) > 1 && os.Args[1] == "add-server":
		err = addServer(os.Args[2:])
	default:
		err = run(log, os.Args[1:])
	}
	if err != nil {
		log.Error("ekpanel", "err", err)
		os.Exit(1)
	}
}

// addServer registers a server from the command line and prints its one-time enrolment
// token and the agent CA pin. The installer uses it to connect the panel's own server before any admin exists.
//
//	ekpanel add-server --name "This server" [panel flags such as --database-url]
func addServer(args []string) error {
	name := "This server"
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		default:
			rest = append(rest, args[i])
		}
	}
	cfg, err := config.Load(rest)
	if err != nil {
		return err
	}
	if cfg.DevDB {
		return fmt.Errorf("add-server needs --database-url; the embedded dev database only runs inside the panel process")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	st := store.New(pool)
	token := auth.NewToken()
	srv, err := st.CreateServer(ctx, name, auth.HashToken(token))
	if err != nil {
		return err
	}
	if err := st.AddAudit(ctx, nil, "", "server.created", srv.Name, map[string]any{"server_id": srv.ID, "via": "cli"}); err != nil {
		return err
	}
	ca, err := pki.LoadOrCreateCA(filepath.Join(cfg.DataDir, "agent-ca"))
	if err != nil {
		return err
	}
	// One line, "<token> <ca-pin>", for scripts such as the installer.
	fmt.Println(token, ca.Pin())
	return nil
}

func run(log *slog.Logger, args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := cfg.DatabaseURL
	if cfg.DevDB {
		log.Warn("using the embedded development database; do not use --dev-db in production")
		logFile, err := os.OpenFile(filepath.Join(cfg.DataDir, "devdb.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logFile.Close()
		dev, err := db.StartDevDB(filepath.Join(cfg.DataDir, "devdb"), "", logFile)
		if err != nil {
			return err
		}
		defer dev.Stop()
		dbURL = dev.URL
	}

	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}
	st := store.New(pool)

	ca, err := pki.LoadOrCreateCA(filepath.Join(cfg.DataDir, "agent-ca"))
	if err != nil {
		return fmt.Errorf("agent CA: %w", err)
	}
	agentTLS, err := ca.ServerTLSConfig()
	if err != nil {
		return err
	}
	_, portStr, err := net.SplitHostPort(cfg.AgentListen)
	if err != nil {
		return fmt.Errorf("--agent-listen: %w", err)
	}
	agentPort, _ := strconv.Atoi(portStr)

	h := hub.New()
	runner := tasks.New(st, h, log)
	box, err := secretbox.LoadOrCreate(filepath.Join(cfg.DataDir, "secret.key"))
	if err != nil {
		return fmt.Errorf("secret key: %w", err)
	}
	notifier := notify.New(st, box, log)
	runner.SetNotifier(notifier)
	runner.SetSecrets(box)
	opts := api.Options{
		SecureCookies: cfg.SecureCookies, UI: webui.FS(), Version: version,
		CA: ca, AgentPort: agentPort, Tasks: runner,
	}
	if err := prepareSetup(ctx, log, st, cfg.DataDir, &opts); err != nil {
		return err
	}
	opts.Notify, opts.Secrets = notifier, box
	apiServer := api.New(st, h, log, opts)

	web := &http.Server{
		Addr:              cfg.Listen,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: live WebSocket streams stay open for as long as a page is open.
	}
	agents := &http.Server{
		Addr:              cfg.AgentListen,
		Handler:           apiServer.AgentHandler(),
		TLSConfig:         agentTLS,
		ReadHeaderTimeout: 10 * time.Second,
	}

	var background sync.WaitGroup
	background.Add(3)
	go func() { defer background.Done(); runner.Run(ctx, 4) }()
	go func() { defer background.Done(); notifier.Run(ctx) }()
	go func() { defer background.Done(); cleanupLoop(ctx, log, st) }()

	errc := make(chan error, 2)
	go func() {
		log.Info("web UI and API listening", "addr", cfg.Listen, "version", version)
		errc <- web.ListenAndServe()
	}()
	go func() {
		log.Info("agent listener (mutual TLS) listening", "addr", cfg.AgentListen, "ca_pin", ca.Pin())
		errc <- agents.ListenAndServeTLS("", "")
	}()

	var runErr error
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
		stop()
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	web.Shutdown(shutdownCtx)
	agents.Shutdown(shutdownCtx)
	background.Wait()
	return runErr
}

// prepareSetup creates (or reuses) the one-time setup token while the panel has no users.
// The token is printed to the log and stored in <data-dir>/setup-token for the installer.
func prepareSetup(ctx context.Context, log *slog.Logger, st *store.Store, dataDir string, opts *api.Options) error {
	has, err := st.HasUsers(ctx)
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, "setup-token")
	if has {
		os.Remove(path)
		return nil
	}
	token := ""
	if b, err := os.ReadFile(path); err == nil {
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		token = auth.NewToken()
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			return fmt.Errorf("write setup token: %w", err)
		}
	}
	opts.SetupToken = token
	opts.OnSetupDone = func() { os.Remove(path) }
	log.Info("no administrator yet: open the panel and create one with this setup token",
		"setup_token", token, "also_saved_in", path)
	return nil
}

// cleanupLoop expires sessions and compacts metric history every five minutes.
func cleanupLoop(ctx context.Context, log *slog.Logger, st *store.Store) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.DeleteExpiredSessions(ctx); err != nil && ctx.Err() == nil {
				log.Error("delete expired sessions", "err", err)
			}
			if err := st.CompactMetrics(ctx); err != nil && ctx.Err() == nil {
				log.Error("compact metrics", "err", err)
			}
		}
	}
}
