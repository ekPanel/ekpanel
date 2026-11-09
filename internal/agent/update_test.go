package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/release"
)

// testRelease serves a signed release whose binaries are shell scripts printing the version.
func testRelease(t *testing.T, priv ed25519.PrivateKey, version, reports string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range []string{"ekpanel", "ekpanel-agent"} {
		body := "#!/bin/sh\necho " + reports + "\n"
		tw.WriteHeader(&tar.Header{Name: "ekpanel-" + version + "-linux-amd64/" + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), release.ArchiveName(version, "amd64"))
	files := map[string][]byte{
		"SHA256SUMS":                          []byte(sums),
		"SHA256SUMS.sig":                      ed25519.Sign(priv, []byte(sums)),
		release.ArchiveName(version, "amd64"): buf.Bytes(),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/download/"+version+"/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

type fakeUpdater struct {
	*updater
	mu       sync.Mutex
	commands []string
	healthV  string
}

func newTestUpdater(t *testing.T, pub ed25519.PublicKey, panelHost bool) *fakeUpdater {
	t.Helper()
	dir := t.TempDir()
	f := &fakeUpdater{}
	f.updater = &updater{
		binDir:    filepath.Join(dir, "bin"),
		etcDir:    filepath.Join(dir, "etc"),
		backupDir: filepath.Join(dir, "backups"),
		arch:      "amd64",
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			f.mu.Lock()
			f.commands = append(f.commands, strings.Join(append([]string{filepath.Base(name)}, args...), " "))
			f.mu.Unlock()
			if len(args) == 1 && args[0] == "version" {
				b, err := os.ReadFile(name)
				if err != nil {
					return nil, err
				}
				// The fake binaries are "#!/bin/sh\necho VERSION".
				return []byte(strings.TrimPrefix(strings.Split(string(b), "\n")[1], "echo ") + "\n"), nil
			}
			return nil, nil
		},
		dumpDB: func(ctx context.Context, path string) error { return os.WriteFile(path, []byte("dump"), 0o600) },
		health: func(ctx context.Context, addr string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.healthV, nil
		},
		client: func(base string) *release.Client {
			return &release.Client{BaseURL: base, Keys: []ed25519.PublicKey{pub}}
		},
		sleep:      func(time.Duration) {},
		healthWait: 50 * time.Millisecond,
		log:        slog.New(slog.DiscardHandler),
	}
	os.MkdirAll(f.binDir, 0o755)
	os.MkdirAll(f.etcDir, 0o755)
	for _, name := range []string{"ekpanel", "ekpanel-agent"} {
		os.WriteFile(filepath.Join(f.binDir, name), []byte("#!/bin/sh\necho v0.1.0\n"), 0o755)
	}
	if panelHost {
		os.WriteFile(filepath.Join(f.etcDir, "panel.env"), []byte("EKPANEL_LISTEN=127.0.0.1:9090\n"), 0o644)
	}
	return f
}

func (f *fakeUpdater) ran(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.HasPrefix(c, cmd) {
			return true
		}
	}
	return false
}

func readVersion(t *testing.T, path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(strings.Split(string(b), "\n")[1], "echo ")
}

func TestUpdateAgent(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	f := newTestUpdater(t, pub, false)
	url := testRelease(t, priv, "v0.2.0", "v0.2.0")
	ctx := context.Background()

	res, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "agent", Version: "v0.2.0", ReleasesURL: url})
	if opErr != nil {
		t.Fatal(opErr)
	}
	a := res.(afterReply)
	if r := a.result.(agentproto.SystemUpdateResult); r.Previous != "v0.1.0" || r.Version != "v0.2.0" {
		t.Fatalf("result = %+v", r)
	}
	if v := readVersion(t, f.path("ekpanel-agent")); v != "v0.2.0" {
		t.Fatalf("installed %s", v)
	}
	if v := readVersion(t, f.path("ekpanel-agent.prev")); v != "v0.1.0" {
		t.Fatalf("kept %s", v)
	}
	if _, err := os.Stat(f.path("ekpanel-agent.new")); err == nil {
		t.Fatal("ekpanel-agent.new left behind")
	}

	a.then()
	if !f.ran("systemd-run --unit=ekpanel-agent-rollback") || !f.ran("systemctl restart --no-block ekpanel-agent") {
		t.Fatalf("commands: %q", f.commands)
	}
	if b, _ := os.ReadFile(f.markerPath()); strings.TrimSpace(string(b)) != "v0.2.0" {
		t.Fatalf("marker = %q", b)
	}
	// The old agent reconnecting does not confirm the update; the new one does.
	f.confirmUpdate("v0.1.0")
	if _, err := os.Stat(f.markerPath()); err != nil {
		t.Fatal("marker removed by the old version")
	}
	f.confirmUpdate("v0.2.0")
	if _, err := os.Stat(f.markerPath()); err == nil {
		t.Fatal("marker still there after the new version connected")
	}

	// Never down, and never to the same version.
	for _, v := range []string{"v0.2.0", "v0.1.5"} {
		if _, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "agent", Version: v, ReleasesURL: url}); opErr == nil || opErr.Code != agentproto.ErrNotAllowed {
			t.Fatalf("update to %s: %v", v, opErr)
		}
	}
}

func TestUpdateRefusesBadDownloads(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ctx := context.Background()

	// A binary that does not report the version it was released as.
	f := newTestUpdater(t, pub, false)
	url := testRelease(t, priv, "v0.2.0", "v0.9.9")
	if _, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "agent", Version: "v0.2.0", ReleasesURL: url}); opErr == nil || !strings.Contains(opErr.Message, "says it is") {
		t.Fatalf("wrong version inside: %v", opErr)
	}
	// Signed by someone else.
	_, other, _ := ed25519.GenerateKey(nil)
	url = testRelease(t, other, "v0.2.0", "v0.2.0")
	if _, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "agent", Version: "v0.2.0", ReleasesURL: url}); opErr == nil || !strings.Contains(opErr.Message, "not signed") {
		t.Fatalf("other key: %v", opErr)
	}
	if v := readVersion(t, f.path("ekpanel-agent")); v != "v0.1.0" {
		t.Fatalf("binary changed to %s", v)
	}
	// The panel only where it runs.
	url = testRelease(t, priv, "v0.2.0", "v0.2.0")
	if _, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "panel", Version: "v0.2.0", ReleasesURL: url}); opErr == nil || opErr.Code != agentproto.ErrNotAllowed {
		t.Fatalf("panel on another server: %v", opErr)
	}
	if _, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "agent", Version: "v0.2.0", ReleasesURL: "file:///etc"}); opErr == nil || opErr.Code != agentproto.ErrInvalidParams {
		t.Fatalf("file URL: %v", opErr)
	}
}

func TestUpdatePanel(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ctx := context.Background()
	url := testRelease(t, priv, "v0.2.0", "v0.2.0")

	t.Run("healthy", func(t *testing.T) {
		f := newTestUpdater(t, pub, true)
		f.healthV = "v0.2.0"
		res, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "panel", Version: "v0.2.0", ReleasesURL: url})
		if opErr != nil {
			t.Fatal(opErr)
		}
		a := res.(afterReply)
		r := a.result.(agentproto.SystemUpdateResult)
		if r.Backup == "" {
			t.Fatal("no database backup")
		}
		if _, err := os.Stat(r.Backup); err != nil {
			t.Fatal(err)
		}
		a.then()
		if v := readVersion(t, f.path("ekpanel")); v != "v0.2.0" {
			t.Fatalf("panel is %s", v)
		}
		if f.panelAddress() != "127.0.0.1:9090" {
			t.Fatalf("address %s", f.panelAddress())
		}
	})

	t.Run("does not start", func(t *testing.T) {
		f := newTestUpdater(t, pub, true)
		f.healthV = "v0.1.0" // the old panel still answers: the new one never came up
		res, opErr := f.update(ctx, agentproto.SystemUpdateParams{Component: "panel", Version: "v0.2.0", ReleasesURL: url})
		if opErr != nil {
			t.Fatal(opErr)
		}
		res.(afterReply).then()
		if v := readVersion(t, f.path("ekpanel")); v != "v0.1.0" {
			t.Fatalf("panel is %s, want the previous version back", v)
		}
		if v := readVersion(t, f.path("ekpanel.failed")); v != "v0.2.0" {
			t.Fatalf("failed binary is %s", v)
		}
	})
}

func TestBackupsKeepNewest(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	f := newTestUpdater(t, pub, true)
	os.MkdirAll(f.backupDir, 0o700)
	for i := range 5 {
		p := filepath.Join(f.backupDir, fmt.Sprintf("ekpanel-db-v0.0.%d-x.dump", i))
		os.WriteFile(p, nil, 0o600)
		os.Chtimes(p, time.Now().Add(-time.Duration(10-i)*time.Hour), time.Now().Add(-time.Duration(10-i)*time.Hour))
	}
	if _, err := f.backupDatabase(context.Background(), "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(f.backupDir)
	if len(entries) != keepBackups {
		t.Fatalf("%d dumps kept, want %d", len(entries), keepBackups)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "v0.0.0") || strings.Contains(e.Name(), "v0.0.1") || strings.Contains(e.Name(), "v0.0.2") {
			t.Fatalf("kept an old dump: %s", e.Name())
		}
	}
}
