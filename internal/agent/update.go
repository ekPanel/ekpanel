package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/release"
)

// Self-update of the panel and the agent (see agentproto.OpSystemUpdate).
//
// The new binary is downloaded and checked (release.Client: signed SHA256SUMS), written
// next to the old one as NAME.new and run once ("NAME version" must print the release's
// version). Then the old binary becomes NAME.prev and the new one takes its place.
//
// Before a panel update the panel database is dumped to backupDir. After the reply the
// panel is restarted; if its health check does not show the new version within
// panelHealthWait, the old binary is put back and the panel restarted again.
//
// The agent cannot watch its own restart, so before restarting it starts a systemd timer
// (rollbackUnit) that puts the old binary back after agentRollbackAfter, unless the new
// agent has removed the marker file by then: it does so as soon as it reaches the panel.

const (
	panelHealthWait    = 90 * time.Second
	agentRollbackAfter = 3 * time.Minute
	rollbackUnit       = "ekpanel-agent-rollback"
	keepBackups        = 3
)

type updater struct {
	binDir    string // /usr/local/bin
	etcDir    string // /etc/ekpanel
	backupDir string // /var/backups/ekpanel
	arch      string
	// run runs a command and returns its output.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// dumpDB writes the panel database to path (pg_dump's custom format).
	dumpDB func(ctx context.Context, path string) error
	// health asks the panel at addr for its version.
	health func(ctx context.Context, addr string) (string, error)
	// client downloads releases; nil uses one for the params' URL.
	client func(baseURL string) *release.Client
	sleep  func(time.Duration)
	// healthWait is how long the restarted panel has to show its new version.
	healthWait time.Duration
	// log is nil on a real server: the agent's default logger is used.
	log *slog.Logger
}

func (u *updater) logger() *slog.Logger {
	if u.log != nil {
		return u.log
	}
	return slog.Default()
}

var realUpdater = &updater{
	binDir:    "/usr/local/bin",
	etcDir:    "/etc/ekpanel",
	backupDir: "/var/backups/ekpanel",
	arch:      runtime.GOARCH,
	run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return runCommand(ctx, nil, name, args...)
	},
	dumpDB: func(ctx context.Context, path string) error {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd := exec.CommandContext(ctx, "runuser", "-u", "postgres", "--", "pg_dump", "-Fc", "ekpanel")
		cmd.Stdout = f
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			os.Remove(path)
			return fmt.Errorf("pg_dump: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
		}
		return f.Sync()
	},
	health:     panelVersion,
	client:     func(baseURL string) *release.Client { return &release.Client{BaseURL: baseURL} },
	sleep:      time.Sleep,
	healthWait: panelHealthWait,
}

func (u *updater) path(name string) string { return filepath.Join(u.binDir, name) }

// panelHost reports whether this server runs the panel (the installer's panel mode).
func (u *updater) panelHost() bool {
	_, errEnv := os.Stat(filepath.Join(u.etcDir, "panel.env"))
	_, errBin := os.Stat(u.path("ekpanel"))
	return errEnv == nil && errBin == nil
}

func (u *updater) markerPath() string { return filepath.Join(u.etcDir, "agent-update.pending") }

// afterReply is a result whose then runs once the reply has been sent.
type afterReply struct {
	result any
	then   func()
}

func (u *updater) update(ctx context.Context, p agentproto.SystemUpdateParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	name := "ekpanel-agent"
	if p.Component == agentproto.ComponentPanel {
		if !u.panelHost() {
			return nil, opError(agentproto.ErrNotAllowed, "the panel does not run on this server")
		}
		name = "ekpanel"
	}
	current := u.version(ctx, u.path(name))
	if release.Compare(p.Version, current) <= 0 {
		return nil, opError(agentproto.ErrNotAllowed, "%s is already at %s; updates only go to newer versions", name, current)
	}

	data, err := u.client(p.ReleasesURL).Binary(ctx, p.Version, u.arch, name)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "download %s %s: %v", name, p.Version, err)
	}
	next := u.path(name + ".new")
	if err := os.WriteFile(next, data, 0o755); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if got := u.version(ctx, next); got != p.Version {
		os.Remove(next)
		return nil, opError(agentproto.ErrFailed, "the downloaded %s says it is %q, not %s; not installing it", name, got, p.Version)
	}

	res := agentproto.SystemUpdateResult{Previous: current, Version: p.Version}
	if p.Component == agentproto.ComponentPanel {
		if res.Backup, err = u.backupDatabase(ctx, current); err != nil {
			os.Remove(next)
			return nil, opError(agentproto.ErrFailed, "back up the panel database first: %v", err)
		}
	}
	if err := u.swap(name); err != nil {
		os.Remove(next)
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	u.logger().Info("installed update", "binary", name, "from", current, "to", p.Version)

	if p.Component == agentproto.ComponentPanel {
		return afterReply{result: res, then: func() { u.restartPanel(p.Version) }}, nil
	}
	return afterReply{result: res, then: func() { u.restartAgent(p.Version) }}, nil
}

// version runs "BINARY version" and returns what it prints ("" when it does not run).
func (u *updater) version(ctx context.Context, bin string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := u.run(ctx, bin, "version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// swap makes NAME.new the program, keeping the old one as NAME.prev. Renames within one
// folder are atomic, so a crash leaves either the old or the new binary in place.
func (u *updater) swap(name string) error {
	cur, prev := u.path(name), u.path(name+".prev")
	if _, err := os.Stat(cur); err == nil {
		os.Remove(prev)
		if err := os.Link(cur, prev); err != nil {
			return fmt.Errorf("keep the old %s: %w", name, err)
		}
	}
	return os.Rename(u.path(name+".new"), cur)
}

// restore puts NAME.prev back, keeping the new one as NAME.failed for a look later.
func (u *updater) restore(name string) error {
	cur, prev := u.path(name), u.path(name+".prev")
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous %s to go back to", name)
	}
	os.Remove(u.path(name + ".failed"))
	os.Link(cur, u.path(name+".failed"))
	return os.Rename(prev, cur)
}

func (u *updater) backupDatabase(ctx context.Context, from string) (string, error) {
	if err := os.MkdirAll(u.backupDir, 0o700); err != nil {
		return "", err
	}
	if from == "" {
		from = "unknown"
	}
	path := filepath.Join(u.backupDir, fmt.Sprintf("ekpanel-db-%s-%s.dump", from, time.Now().UTC().Format("20060102-150405")))
	if err := u.dumpDB(ctx, path); err != nil {
		return "", err
	}
	// Keep the newest few.
	var dumps []string
	entries, _ := os.ReadDir(u.backupDir)
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), "ekpanel-db-") && strings.HasSuffix(e.Name(), ".dump") {
			dumps = append(dumps, e.Name())
		}
	}
	slices.SortFunc(dumps, func(a, b string) int { return modTime(u.backupDir, b).Compare(modTime(u.backupDir, a)) })
	for _, old := range dumps[min(keepBackups, len(dumps)):] {
		os.Remove(filepath.Join(u.backupDir, old))
	}
	return path, nil
}

func modTime(dir, name string) time.Time {
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// restartPanel restarts the panel and checks that the new version answers; otherwise it
// goes back to the previous binary.
func (u *updater) restartPanel(version string) {
	ctx, cancel := context.WithTimeout(context.Background(), u.healthWait+time.Minute)
	defer cancel()
	u.sleep(time.Second) // let the reply reach the panel before it stops
	if _, err := u.run(ctx, "systemctl", "restart", "ekpanel"); err != nil {
		u.logger().Error("restart the panel after the update", "err", err)
	}
	addr := u.panelAddress()
	deadline := time.Now().Add(u.healthWait)
	var got string
	for time.Now().Before(deadline) {
		hctx, hcancel := context.WithTimeout(ctx, 5*time.Second)
		v, err := u.health(hctx, addr)
		hcancel()
		if err == nil {
			got = v
			if v == version {
				u.logger().Info("the updated panel is running", "version", version)
				return
			}
		}
		u.sleep(2 * time.Second)
	}
	u.logger().Error("the updated panel did not start; going back to the previous version", "version", version, "answered", got)
	if err := u.restore("ekpanel"); err != nil {
		u.logger().Error("go back to the previous panel", "err", err)
		return
	}
	if _, err := u.run(ctx, "systemctl", "restart", "ekpanel"); err != nil {
		u.logger().Error("restart the previous panel", "err", err)
	}
}

// panelAddress is where the panel's HTTP server listens (EKPANEL_LISTEN in panel.env).
func (u *updater) panelAddress() string {
	addr := "127.0.0.1:8080"
	f, err := os.Open(filepath.Join(u.etcDir, "panel.env"))
	if err != nil {
		return addr
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "EKPANEL_LISTEN="); ok {
			addr = strings.Trim(v, `"'`)
		}
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func panelVersion(ctx context.Context, addr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/v1/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil || resp.StatusCode != http.StatusOK || body.Status != "ok" {
		return "", fmt.Errorf("panel health: %s", resp.Status)
	}
	return body.Version, nil
}

// restartAgent arms the rollback timer and restarts the agent service, which ends this
// process.
func (u *updater) restartAgent(version string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := u.armRollback(ctx, version); err != nil {
		// Without the safety net, a broken agent could not be reached again: go back now.
		u.logger().Error("set up the rollback timer; not switching to the new agent", "err", err)
		if err := u.restore("ekpanel-agent"); err != nil {
			u.logger().Error("go back to the previous agent", "err", err)
		}
		return
	}
	u.sleep(time.Second) // let the reply reach the panel
	// systemd stops this process (and the systemctl it started) while restarting the unit,
	// so a "terminated" error here is the restart happening.
	if _, err := u.run(ctx, "systemctl", "restart", "--no-block", "ekpanel-agent"); err != nil && !strings.Contains(err.Error(), "signal: terminated") {
		u.logger().Error("restart the agent after the update", "err", err)
	}
}

func (u *updater) armRollback(ctx context.Context, version string) error {
	if err := os.WriteFile(u.markerPath(), []byte(version+"\n"), 0o600); err != nil {
		return err
	}
	// A timer left from an earlier update would get in the way of the new one.
	u.run(ctx, "systemctl", "stop", rollbackUnit+".timer", rollbackUnit+".service")
	u.run(ctx, "systemctl", "reset-failed", rollbackUnit+".service")
	marker, cur, prev := u.markerPath(), u.path("ekpanel-agent"), u.path("ekpanel-agent.prev")
	script := fmt.Sprintf(`if [ -e %[1]s ]; then rm -f %[1]s; mv -f %[2]s %[3]s.failed; mv -f %[4]s %[2]s; systemctl restart ekpanel-agent; fi`,
		shellQuote(marker), shellQuote(cur), shellQuote(cur), shellQuote(prev))
	_, err := u.run(ctx, "systemd-run", "--unit="+rollbackUnit, "--description=Put the previous ekPanel agent back if the update failed",
		fmt.Sprintf("--on-active=%ds", int(agentRollbackAfter.Seconds())), "--timer-property=AccuracySec=1s",
		"/bin/sh", "-c", script)
	return err
}

// confirmUpdate runs once the agent has reached the panel: an update to this version
// worked, so the rollback timer must not fire.
func (u *updater) confirmUpdate(version string) {
	b, err := os.ReadFile(u.markerPath())
	if errors.Is(err, fs.ErrNotExist) || err != nil {
		return
	}
	if strings.TrimSpace(string(b)) != version {
		return
	}
	if err := os.Remove(u.markerPath()); err == nil {
		u.logger().Info("update confirmed", "version", version)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u.run(ctx, "systemctl", "stop", rollbackUnit+".timer")
}
