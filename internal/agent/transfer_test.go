package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

const testIssuer = "acme-v02.api.letsencrypt.org-directory"

// transferSystem runs real tar inside a temporary root and fakes the database tools.
func transferSystem(t *testing.T, loaded *[]string) *system {
	t.Helper()
	root := t.TempDir()
	s := &system{root: root}
	s.exec = func(ctx context.Context, c command) ([]byte, error) {
		switch {
		case c.name == "mysqldump":
			io.WriteString(c.stdout, "CREATE TABLE posts (id int);\nINSERT INTO posts VALUES (1);\n")
			return nil, nil
		case c.name == "runuser" && slices.Contains(c.args, "pg_dump"):
			io.WriteString(c.stdout, "CREATE TABLE notes (id int);\n")
			return nil, nil
		case c.name == "mysql" || (c.name == "runuser" && slices.Contains(c.args, "psql")):
			b, _ := io.ReadAll(c.stdin)
			*loaded = append(*loaded, c.name+": "+string(b))
			return nil, nil
		}
		cmd := exec.CommandContext(ctx, c.name, c.args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
		return nil, cmd.Run()
	}
	s.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "chown" {
			return nil, nil
		}
		if name == "tar" {
			// Tests do not run as root: keep owners as they come.
			args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--same-owner" })
		}
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	return s
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTransferRoundTrip(t *testing.T) {
	var loaded []string
	src, dst := transferSystem(t, &loaded), transferSystem(t, &loaded)
	writeFile(t, src.path("/home/shop_1/public_html/index.php"), "<?php echo 'hi';")
	writeFile(t, src.path("/home/shop_1/public_html/wp-content/uploads/a.jpg"), strings.Repeat("x", 300<<10))
	certs := filepath.Join(caddyCertDir, testIssuer, "shop.example.com")
	for _, f := range []string{"shop.example.com.crt", "shop.example.com.key", "shop.example.com.json"} {
		writeFile(t, src.path(filepath.Join(certs, f)), "cert "+f)
	}
	writeFile(t, src.path(filepath.Join(caddyCertDir, testIssuer, "other.example.com", "other.example.com.crt")), "not this site's")

	// Export into a buffer, then receive it on the target.
	var buf bytes.Buffer
	p := agentproto.TransferExportParams{Stream: "s1", User: "shop_1", Domains: []string{"shop.example.com", "www.shop.example.com"},
		Databases: []agentproto.TransferDatabase{{Engine: "mariadb", Name: "shop"}, {Engine: "postgresql", Name: "notes"}}}
	sent, err := src.export(context.Background(), &buf, p)
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Files || sent.Databases != 2 || sent.Certs != 3 {
		t.Fatalf("export result %+v", sent)
	}
	move := "11111111-2222-3333-4444-555555555555"
	dir := dst.path(filepath.Join(agentproto.IncomingDir, move))
	os.MkdirAll(dir, 0o700)
	got, err := dst.receive(bytes.NewReader(buf.Bytes()), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bytes != sent.Bytes || got.Certs != 3 || got.Databases != 2 || !got.Files {
		t.Fatalf("receive result %+v, sent %+v", got, sent)
	}
	if b, _ := os.ReadFile(dst.path(filepath.Join(certs, "shop.example.com.key"))); string(b) != "cert shop.example.com.key" {
		t.Fatalf("certificate not in the target's Caddy storage: %q", b)
	}
	if _, err := os.Stat(dst.path(filepath.Join(caddyCertDir, testIssuer, "other.example.com"))); err == nil {
		t.Fatal("another site's certificate was copied")
	}

	// The target made the account with a placeholder page; the copy replaces it.
	writeFile(t, dst.path("/home/shop_1/public_html/index.html"), "placeholder")
	dbs := []agentproto.TransferDatabase{{Engine: "mariadb", Name: "shop", User: "shop"}, {Engine: "postgresql", Name: "notes", User: "notes_u"}}
	if _, opErr := transferFinish(context.Background(), dst, agentproto.TransferFinishParams{Move: move, User: "shop_1", Databases: dbs}); opErr != nil {
		t.Fatal(opErr)
	}
	if b, _ := os.ReadFile(dst.path("/home/shop_1/public_html/index.php")); string(b) != "<?php echo 'hi';" {
		t.Fatalf("files not in place: %q", b)
	}
	if _, err := os.Stat(dst.path("/home/shop_1/public_html/index.html")); err == nil {
		t.Fatal("the placeholder page is still there")
	}
	if len(loaded) != 2 || !strings.Contains(loaded[0], "INSERT INTO posts") || !strings.HasPrefix(loaded[1], "runuser: SET ROLE notes_u;\nCREATE TABLE notes") {
		t.Fatalf("dumps loaded: %q", loaded)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("the incoming folder was not removed")
	}
	// Finishing twice: the copy is gone.
	if _, opErr := transferFinish(context.Background(), dst, agentproto.TransferFinishParams{Move: move, User: "shop_1"}); opErr == nil {
		t.Fatal("finished a move whose copy is gone")
	}
}

func TestTransferRefusesBadData(t *testing.T) {
	var loaded []string
	src, dst := transferSystem(t, &loaded), transferSystem(t, &loaded)
	writeFile(t, src.path("/home/shop_1/public_html/index.php"), "hi")
	var buf bytes.Buffer
	if _, err := src.export(context.Background(), &buf, agentproto.TransferExportParams{Stream: "s", User: "shop_1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Cut short: no end section.
	if _, err := dst.receive(bytes.NewReader(buf.Bytes()[:buf.Len()/2]), dir); err == nil {
		t.Fatal("a cut-off transfer was accepted")
	}
	if _, err := dst.receive(strings.NewReader("not gzip"), dir); err == nil {
		t.Fatal("garbage was accepted")
	}

	// A home archive with a file outside the account's folder.
	move := "11111111-2222-3333-4444-555555555556"
	in := dst.path(filepath.Join(agentproto.IncomingDir, move))
	os.MkdirAll(in, 0o700)
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for _, name := range []string{"shop_1/public_html/x", "other/evil"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
	}
	tw.Close()
	os.WriteFile(filepath.Join(in, "home.tar"), tb.Bytes(), 0o600)
	os.WriteFile(filepath.Join(in, "user"), []byte("shop_1"), 0o600)
	os.WriteFile(filepath.Join(in, "complete"), nil, 0o600)
	_, opErr := transferFinish(context.Background(), dst, agentproto.TransferFinishParams{Move: move, User: "shop_1"})
	if opErr == nil || !strings.Contains(opErr.Message, "outside the site's folder") {
		t.Fatalf("archive with a foreign path: %v", opErr)
	}
	if _, err := os.Stat(dst.path("/home/other/evil")); err == nil {
		t.Fatal("a file outside the site was written")
	}
	// Files copied for one account cannot be put into another.
	if _, opErr := transferFinish(context.Background(), dst, agentproto.TransferFinishParams{Move: move, User: "shop_2"}); opErr == nil {
		t.Fatal("finished into another account")
	}
}

// TestTransferFlowControl checks that an export sends only as many chunks as it has credit
// for, and that an import grants credit as it writes.
func TestTransferFlowControl(t *testing.T) {
	var loaded []string
	src := transferSystem(t, &loaded)
	// Random-ish data that gzip cannot shrink much: several chunks.
	var big strings.Builder
	for i := range 40000 {
		big.WriteString(strings.Repeat(string(rune('a'+i%26)), i%7+1))
		big.WriteString(time.Duration(i * 7919).String())
	}
	writeFile(t, src.path("/home/shop_1/public_html/big.txt"), big.String())

	var mu sync.Mutex
	var envs []agentproto.Envelope
	done := make(chan agentproto.Envelope, 1)
	tr := newTransfers(src, func(env agentproto.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		if env.Type == agentproto.TypeStreamClose {
			done <- env
			return nil
		}
		envs = append(envs, env)
		return nil
	})
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(envs)
	}
	if opErr := tr.startExport(context.Background(), agentproto.TransferExportParams{Stream: "x1", User: "shop_1"}); opErr != nil {
		t.Fatal(opErr)
	}
	tr.credit(agentproto.Envelope{Stream: "x1", Credit: 2})
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("sent %d chunks with credit for 2", n)
	}
	for count() < 1000 {
		select {
		case env := <-done:
			if env.Reason != "done" {
				t.Fatalf("export ended: %s", env.Reason)
			}
			var res agentproto.TransferResult
			json.Unmarshal(env.Result, &res)
			if res.Bytes == 0 || !res.Files {
				t.Fatalf("result %+v", res)
			}
			// Feed everything to an import on another system and check it arrives whole.
			dst := transferSystem(t, &loaded)
			var credits int
			var dmu sync.Mutex
			finished := make(chan agentproto.Envelope, 1)
			im := newTransfers(dst, func(env agentproto.Envelope) error {
				switch env.Type {
				case agentproto.TypeStreamCredit:
					dmu.Lock()
					credits += env.Credit
					dmu.Unlock()
				case agentproto.TypeStreamClose:
					finished <- env
				}
				return nil
			})
			move := "11111111-2222-3333-4444-555555555557"
			if opErr := im.startImport(context.Background(), agentproto.TransferImportParams{Stream: "i1", Move: move}); opErr != nil {
				t.Fatal(opErr)
			}
			mu.Lock()
			chunks := slices.Clone(envs)
			mu.Unlock()
			for _, c := range chunks {
				for {
					dmu.Lock()
					ok := credits > 0
					if ok {
						credits--
					}
					dmu.Unlock()
					if ok {
						break
					}
					time.Sleep(time.Millisecond)
				}
				im.input(agentproto.Envelope{Stream: "i1", Data: c.Data})
			}
			im.close(agentproto.Envelope{Stream: "i1", Reason: "eof"})
			end := <-finished
			var got agentproto.TransferResult
			json.Unmarshal(end.Result, &got)
			if end.Reason != "done" || got.Bytes != res.Bytes {
				t.Fatalf("import ended %q with %+v, export sent %+v", end.Reason, got, res)
			}
			return
		default:
			tr.credit(agentproto.Envelope{Stream: "x1", Credit: 8})
			time.Sleep(5 * time.Millisecond)
		}
	}
	t.Fatal("the export never finished")
}
