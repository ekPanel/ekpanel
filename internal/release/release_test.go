package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	ordered := []string{"dev", "v0.1.0-alpha", "v0.1.0-alpha.1", "v0.1.0-alpha.beta", "v0.1.0-beta.2", "v0.1.0-beta.11", "v0.1.0-rc.1", "v0.1.0", "v0.1.1", "v0.2.0", "v1.0.0", "v1.10.0"}
	for i := range ordered {
		for j := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(ordered[i], ordered[j]); got != want {
				t.Errorf("Compare(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	for _, bad := range []string{"", "dev", "1.2.3", "v1.2", "v01.2.3", "v1.2.3-", "v1.2.3-a_b", "v1.2.3/../x"} {
		if Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
}

func TestBuiltInKey(t *testing.T) {
	if len(Keys()) != 1 {
		t.Fatalf("want the built-in release key, got %d keys", len(Keys()))
	}
}

// fakeRelease serves a signed release the way GitHub lays it out.
func fakeRelease(t *testing.T, priv ed25519.PrivateKey, version string, tamper bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	dir := fmt.Sprintf("ekpanel-%s-linux-amd64/", version)
	for name, body := range map[string]string{"ekpanel": "panel " + version, "ekpanel-agent": "agent " + version, "LICENSE": "AGPL"} {
		tw.WriteHeader(&tar.Header{Name: dir + name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), ArchiveName(version, "amd64"))
	sig := ed25519.Sign(priv, []byte(sums))
	if tamper {
		archive = append(archive, 0)
	}
	files := map[string][]byte{
		"/download/" + version + "/SHA256SUMS":                       []byte(sums),
		"/download/" + version + "/SHA256SUMS.sig":                   sig,
		"/download/" + version + "/" + ArchiveName(version, "amd64"): archive,
		"/latest/download/release.json":                              []byte(`{"version":"` + version + `"}`),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBinary(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	ctx := context.Background()

	srv := fakeRelease(t, priv, "v0.2.0", false)
	c := &Client{BaseURL: srv.URL, Keys: []ed25519.PublicKey{pub}}
	info, err := c.Latest(ctx)
	if err != nil || info.Version != "v0.2.0" {
		t.Fatalf("Latest = %+v, %v", info, err)
	}
	b, err := c.Binary(ctx, "v0.2.0", "amd64", "ekpanel-agent")
	if err != nil || string(b) != "agent v0.2.0" {
		t.Fatalf("Binary = %q, %v", b, err)
	}

	// Signed by another key.
	other, _, _ := ed25519.GenerateKey(nil)
	if _, err := (&Client{BaseURL: srv.URL, Keys: []ed25519.PublicKey{other}}).Binary(ctx, "v0.2.0", "amd64", "ekpanel"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("wrong key: %v", err)
	}
	// The archive was changed after signing.
	bad := fakeRelease(t, priv, "v0.2.0", true)
	if _, err := (&Client{BaseURL: bad.URL, Keys: []ed25519.PublicKey{pub}}).Binary(ctx, "v0.2.0", "amd64", "ekpanel"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered archive: %v", err)
	}
	// No such architecture, and names that are not ours.
	if _, err := c.Binary(ctx, "v0.2.0", "arm64", "ekpanel"); err == nil {
		t.Fatal("missing arch: no error")
	}
	if _, err := c.Binary(ctx, "v0.2.0", "amd64", "LICENSE"); err == nil {
		t.Fatal("other file: no error")
	}
	if _, err := c.Binary(ctx, "../x", "amd64", "ekpanel"); err == nil {
		t.Fatal("bad version: no error")
	}
}

func TestVerifyBase64Signature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sums := []byte("abc  file\n")
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, sums))
	if err := Verify(sums, []byte(sig+"\n"), []ed25519.PublicKey{pub}); err != nil {
		t.Fatal(err)
	}
}
