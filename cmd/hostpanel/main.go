// Command hostpanel runs the control panel: web UI, API and the endpoint agents connect to.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"hostpanel/internal/api"
	"hostpanel/internal/auth"
	"hostpanel/internal/config"
	"hostpanel/internal/db"
	"hostpanel/internal/hub"
	"hostpanel/internal/store"
	"hostpanel/internal/webui"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, os.Args[1:]); err != nil {
		log.Error("hostpanel stopped", "err", err)
		os.Exit(1)
	}
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

	opts := api.Options{SecureCookies: cfg.SecureCookies, UI: webui.FS(), Version: version}
	if err := prepareSetup(ctx, log, st, cfg.DataDir, &opts); err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(st, hub.New(), log, opts).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: live WebSocket streams stay open for as long as a page is open.
	}

	go cleanupLoop(ctx, log, st)

	errc := make(chan error, 1)
	go func() {
		log.Info("hostpanel listening", "addr", cfg.Listen, "version", version)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
	return nil
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

func cleanupLoop(ctx context.Context, log *slog.Logger, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.DeleteExpiredSessions(ctx); err != nil {
				log.Error("delete expired sessions", "err", err)
			}
		}
	}
}
