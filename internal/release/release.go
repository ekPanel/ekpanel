// Package release finds, downloads and checks ekPanel releases.
//
// A release is a set of files at <base>/download/<version>/ (GitHub's layout):
//
//	ekpanel-<version>-linux-<arch>.tar.gz   the panel and agent binaries
//	SHA256SUMS                              the SHA-256 of every file
//	SHA256SUMS.sig                          an Ed25519 signature of SHA256SUMS
//	release.json                            {"version": ...}, also at <base>/latest/download/
//
// Only SHA256SUMS is signed; every other file is checked against it, so a download from
// anywhere (a mirror, a proxy) is safe to use once Fetch returns it.
package release

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is where official releases are published.
const DefaultBaseURL = "https://github.com/ekPanel/ekpanel/releases"

//go:embed release.pub
var releasePub []byte

// extraKey is an additional trusted public key (PEM, base64-encoded) for test builds:
// go build -ldflags "-X ekpanel/internal/release.extraKey=...". Official builds leave it empty.
var extraKey string

// Keys are the public keys a release signature may be made with.
func Keys() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	if k, err := ParseKey(releasePub); err == nil {
		keys = append(keys, k)
	}
	if extraKey != "" {
		if b, err := decodeBase64(extraKey); err == nil {
			if k, err := ParseKey(b); err == nil {
				keys = append(keys, k)
			}
		}
	}
	return keys
}

// ParseKey reads a PEM "PUBLIC KEY" holding an Ed25519 key, as openssl writes it.
func ParseKey(pemData []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("no PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return ek, nil
}

// Info describes a release, as in release.json.
type Info struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	NotesURL    string    `json:"notes_url,omitempty"`
}

// Client downloads releases. The zero value uses DefaultBaseURL and the default HTTP client.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Keys overrides Keys() (tests).
	Keys []ed25519.PublicKey
}

// Maximum sizes of downloads, so a broken mirror cannot fill memory.
const (
	maxSmall   = 1 << 20
	maxArchive = 512 << 20
)

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return DefaultBaseURL
}

func (c *Client) keys() []ed25519.PublicKey {
	if c.Keys != nil {
		return c.Keys
	}
	return Keys()
}

func (c *Client) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ekpanel-updater")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// Latest returns the newest published release.
func (c *Client) Latest(ctx context.Context) (Info, error) {
	b, err := c.get(ctx, c.base()+"/latest/download/release.json", maxSmall)
	if err != nil {
		return Info{}, err
	}
	var info Info
	if err := json.Unmarshal(b, &info); err != nil {
		return Info{}, fmt.Errorf("release.json: %w", err)
	}
	if !Valid(info.Version) {
		return Info{}, fmt.Errorf("release.json: %q is not a version", info.Version)
	}
	return info, nil
}

// ArchiveName is the file holding the binaries for one architecture (amd64, arm64).
func ArchiveName(version, arch string) string {
	return fmt.Sprintf("ekpanel-%s-linux-%s.tar.gz", version, arch)
}

// Binary downloads the release's archive for arch, checks it against the signed
// SHA256SUMS, and returns the named binary from it ("ekpanel" or "ekpanel-agent").
func (c *Client) Binary(ctx context.Context, version, arch, name string) ([]byte, error) {
	if !Valid(version) {
		return nil, fmt.Errorf("%q is not a version", version)
	}
	if name != "ekpanel" && name != "ekpanel-agent" {
		return nil, fmt.Errorf("unknown binary %q", name)
	}
	dir := c.base() + "/download/" + version + "/"
	sums, err := c.get(ctx, dir+"SHA256SUMS", maxSmall)
	if err != nil {
		return nil, err
	}
	sig, err := c.get(ctx, dir+"SHA256SUMS.sig", maxSmall)
	if err != nil {
		return nil, err
	}
	if err := Verify(sums, sig, c.keys()); err != nil {
		return nil, err
	}
	archive := ArchiveName(version, arch)
	want, err := SumOf(sums, archive)
	if err != nil {
		return nil, err
	}
	data, err := c.get(ctx, dir+archive, maxArchive)
	if err != nil {
		return nil, err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match SHA256SUMS", archive)
	}
	return Extract(data, name)
}

// Verify checks that sig is a valid signature of sums by one of keys. The signature may be
// raw (64 bytes) or base64.
func Verify(sums, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return errors.New("no release signing key is built in")
	}
	raw := sig
	if len(raw) != ed25519.SignatureSize {
		b, err := decodeBase64(string(sig))
		if err != nil || len(b) != ed25519.SignatureSize {
			return errors.New("SHA256SUMS.sig is not an Ed25519 signature")
		}
		raw = b
	}
	for _, k := range keys {
		if ed25519.Verify(k, sums, raw) {
			return nil
		}
	}
	return errors.New("SHA256SUMS is not signed by the ekPanel release key")
}

// SumOf returns the SHA-256 (hex) that sums lists for file.
func SumOf(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", file)
}

// Extract returns one file, found by its base name, from a .tar.gz archive.
func Extract(archive []byte, name string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the archive has no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, maxArchive))
		}
	}
}

// ---- Versions ----

// Valid reports whether v is a release version: vMAJOR.MINOR.PATCH with an optional
// pre-release part (v1.2.3-rc.1).
func Valid(v string) bool {
	_, ok := parse(v)
	return ok
}

type version struct {
	nums [3]int
	pre  string
}

func parse(v string) (version, bool) {
	var out version
	rest, ok := strings.CutPrefix(v, "v")
	if !ok || len(v) > 64 {
		return out, false
	}
	rest, pre, hasPre := strings.Cut(rest, "-")
	if hasPre && pre == "" {
		return out, false
	}
	out.pre = pre
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return out, false
		}
		out.nums[i] = n
	}
	for _, r := range out.pre {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '.') {
			return out, false
		}
	}
	return out, true
}

// Compare returns -1, 0 or 1 as a is older than, the same as, or newer than b. A version
// that is not valid (such as "dev") is older than every valid one.
func Compare(a, b string) int {
	va, oka := parse(a)
	vb, okb := parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range 3 {
		if va.nums[i] != vb.nums[i] {
			if va.nums[i] < vb.nums[i] {
				return -1
			}
			return 1
		}
	}
	return comparePre(va.pre, vb.pre)
}

// comparePre orders pre-releases as semantic versioning does: none is newest; parts are
// compared numerically when both are numbers.
func comparePre(a, b string) int {
	if a == b {
		return 0
	}
	if a == "" {
		return 1
	}
	if b == "" {
		return -1
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil:
			if na != nb {
				if na < nb {
					return -1
				}
				return 1
			}
		case ea == nil:
			return -1
		case eb == nil:
			return 1
		default:
			if c := strings.Compare(pa[i], pb[i]); c != 0 {
				return c
			}
		}
	}
	switch {
	case len(pa) < len(pb):
		return -1
	case len(pa) > len(pb):
		return 1
	}
	return 0
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(strings.TrimSpace(s))
}
