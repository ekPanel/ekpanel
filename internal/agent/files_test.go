package agent

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func newWorker(t *testing.T) *fileWorker {
	root := t.TempDir()
	for _, d := range []string{"public_html", "logs", "tmp", ".trash"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	return &fileWorker{root: root}
}

func (w *fileWorker) must(t *testing.T, p agentproto.FilesParams) *agentproto.FilesResult {
	t.Helper()
	res, err := w.do(p)
	if err != nil {
		t.Fatalf("%s %s: %v", p.Action, p.Path, err)
	}
	return res
}

func TestWriteReadListRename(t *testing.T) {
	w := newWorker(t)
	w.must(t, agentproto.FilesParams{Action: "write", Path: "/public_html/index.php", Data: []byte("<?php echo 1;")})
	if got := w.must(t, agentproto.FilesParams{Action: "read", Path: "/public_html/index.php"}); string(got.Data) != "<?php echo 1;" {
		t.Fatalf("read %q", got.Data)
	}
	w.must(t, agentproto.FilesParams{Action: "mkdir", Path: "/public_html/assets"})
	list := w.must(t, agentproto.FilesParams{Action: "list", Path: "/public_html"})
	if len(list.Entries) != 2 || list.Entries[0].Name != "assets" || list.Entries[0].Type != "dir" {
		t.Fatalf("list = %+v (folders first)", list.Entries)
	}
	if _, err := w.do(agentproto.FilesParams{Action: "rename", Path: "/public_html/assets", To: "/public_html/index.php"}); err == nil || err.Code != "exists" {
		t.Fatalf("rename onto existing: %v", err)
	}
	w.must(t, agentproto.FilesParams{Action: "rename", Path: "/public_html/index.php", To: "/public_html/home.php"})
	if _, err := w.do(agentproto.FilesParams{Action: "rename", Path: "/public_html", To: "/www"}); err == nil {
		t.Fatal("public_html must not be movable")
	}
}

func TestPathsCannotLeaveRoot(t *testing.T) {
	w := newWorker(t)
	outside := filepath.Join(filepath.Dir(w.root), "outside-"+filepath.Base(w.root))
	os.WriteFile(outside, []byte("secret"), 0o644)
	defer os.Remove(outside)
	for _, p := range []string{"/../outside-" + filepath.Base(w.root), "../../outside-" + filepath.Base(w.root), "/public_html/../../outside-" + filepath.Base(w.root)} {
		if res, err := w.do(agentproto.FilesParams{Action: "read", Path: p}); err == nil && string(res.Data) == "secret" {
			t.Fatalf("%q escaped the root", p)
		}
	}
}

func TestDeleteGoesToTrash(t *testing.T) {
	w := newWorker(t)
	w.must(t, agentproto.FilesParams{Action: "write", Path: "/public_html/old.html", Data: []byte("x")})
	res := w.must(t, agentproto.FilesParams{Action: "delete", Paths: []string{"/public_html/old.html"}})
	if res.Path != "/.trash" {
		t.Fatalf("delete result %+v", res)
	}
	trash := w.must(t, agentproto.FilesParams{Action: "list", Path: "/.trash"})
	if len(trash.Entries) != 1 || !strings.HasSuffix(trash.Entries[0].Name, "-old.html") {
		t.Fatalf("trash = %+v", trash.Entries)
	}
	// Deleting from the trash is permanent; emptying removes the rest.
	w.must(t, agentproto.FilesParams{Action: "delete", Paths: []string{"/.trash/" + trash.Entries[0].Name}})
	if left := w.must(t, agentproto.FilesParams{Action: "list", Path: "/.trash"}); len(left.Entries) != 0 {
		t.Fatal("item not removed from trash")
	}
	for _, protected := range []string{"/", "/public_html", "/.trash", "/logs"} {
		if _, err := w.do(agentproto.FilesParams{Action: "delete", Paths: []string{protected}}); err == nil {
			t.Errorf("%s must not be deletable", protected)
		}
	}
}

func TestChunkedUpload(t *testing.T) {
	w := newWorker(t)
	part1, part2 := bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 50)
	w.must(t, agentproto.FilesParams{Action: "write_chunk", Path: "/public_html/big.bin", Offset: 0, Data: part1})
	if _, err := w.do(agentproto.FilesParams{Action: "write_chunk", Path: "/public_html/big.bin", Offset: 999, Data: part2}); err == nil || err.Code != "bad_offset" {
		t.Fatalf("out-of-order chunk accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.root, "public_html", "big.bin")); err == nil {
		t.Fatal("file visible before the last chunk")
	}
	w.must(t, agentproto.FilesParams{Action: "write_chunk", Path: "/public_html/big.bin", Offset: 100, Data: part2, Final: true})
	got, _ := os.ReadFile(filepath.Join(w.root, "public_html", "big.bin"))
	if !bytes.Equal(got, append(part1, part2...)) {
		t.Fatalf("uploaded %d bytes", len(got))
	}
	r := w.must(t, agentproto.FilesParams{Action: "read_chunk", Path: "/public_html/big.bin", Offset: 100})
	if !bytes.Equal(r.Data, part2) || !r.EOF || r.Size != 150 {
		t.Fatalf("read_chunk = %d bytes eof=%v size=%d", len(r.Data), r.EOF, r.Size)
	}
}

func zipWith(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, _ := os.Create(path)
	zw := zip.NewWriter(f)
	for name, body := range entries {
		fw, _ := zw.Create(name)
		fw.Write([]byte(body))
	}
	zw.Close()
	f.Close()
}

func TestExtractAndCompress(t *testing.T) {
	w := newWorker(t)
	zipWith(t, filepath.Join(w.root, "public_html", "site.zip"), map[string]string{"index.html": "hi", "css/app.css": "body{}"})
	res := w.must(t, agentproto.FilesParams{Action: "extract", Path: "/public_html/site.zip"})
	if res.Count != 2 {
		t.Fatalf("extracted %d", res.Count)
	}
	if b, _ := os.ReadFile(filepath.Join(w.root, "public_html", "css", "app.css")); string(b) != "body{}" {
		t.Fatal("nested file missing")
	}
	os.Chmod(filepath.Join(w.root, "public_html", "index.html"), 0o600)
	w.must(t, agentproto.FilesParams{Action: "extract", Path: "/public_html/site.zip"}) // again, over existing files
	if info, _ := os.Stat(filepath.Join(w.root, "public_html", "index.html")); info.Mode().Perm() != 0o644 {
		t.Fatalf("extracted files must be readable by the web server, got %o", info.Mode().Perm())
	}
	w.must(t, agentproto.FilesParams{Action: "compress", Paths: []string{"/public_html/css", "/public_html/index.html"}, To: "/public_html/backup.zip"})
	zr, err := zip.OpenReader(filepath.Join(w.root, "public_html", "backup.zip"))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	zr.Close()
	if strings.Join(names, ",") != "css/,css/app.css,index.html" {
		t.Fatalf("archive holds %v", names)
	}
}

func TestZipSlipIsBlocked(t *testing.T) {
	w := newWorker(t)
	zipWith(t, filepath.Join(w.root, "public_html", "evil.zip"), map[string]string{"../../escaped.txt": "pwned"})
	if _, err := w.do(agentproto.FilesParams{Action: "extract", Path: "/public_html/evil.zip"}); err == nil {
		t.Fatal("zip slip archive extracted")
	}
	if _, err := os.Stat(filepath.Join(w.root, "escaped.txt")); err == nil {
		t.Fatal("file written outside the target folder")
	}
}

func TestEditorLimitsAndChmod(t *testing.T) {
	w := newWorker(t)
	big := bytes.Repeat([]byte("x"), agentproto.FileEditLimit+1)
	if _, err := w.do(agentproto.FilesParams{Action: "write", Path: "/public_html/big.txt", Data: big}); err == nil || err.Code != "too_large" {
		t.Fatalf("oversized write: %v", err)
	}
	w.must(t, agentproto.FilesParams{Action: "write", Path: "/public_html/a.sh", Data: []byte("x")})
	w.must(t, agentproto.FilesParams{Action: "chmod", Path: "/public_html/a.sh", Mode: "750"})
	if info, _ := os.Stat(filepath.Join(w.root, "public_html", "a.sh")); info.Mode().Perm() != 0o750 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	for _, bad := range []string{"4755", "999", "rwx"} {
		if _, err := w.do(agentproto.FilesParams{Action: "chmod", Path: "/public_html/a.sh", Mode: bad}); err == nil {
			t.Errorf("mode %q accepted", bad)
		}
	}
}
