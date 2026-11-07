package secretbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	b, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", info.Mode().Perm())
	}
	sealed := b.Seal([]byte("bot123:token"))
	again, _ := LoadOrCreate(path)
	if got, err := again.Open(sealed); err != nil || string(got) != "bot123:token" {
		t.Fatalf("open: %q %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := b.Open(sealed); err == nil {
		t.Fatal("tampered data opened")
	}
	other, _ := LoadOrCreate(filepath.Join(t.TempDir(), "other.key"))
	if _, err := other.Open(b.Seal([]byte("x"))); err == nil {
		t.Fatal("another key opened it")
	}
}
