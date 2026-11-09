package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"ekpanel/internal/agentproto"
)

// Uploaded certificates live in certsDir, named after the site and a hash of their content:
// a new certificate is a new file name, so the web server's configuration changes and it
// reloads. Keys are readable by root and the web server only.
const certsDir = "/etc/ekpanel/certs"

func certPaths(sp agentproto.SiteSpec) (cert, key string) {
	sum := sha256.Sum256([]byte(sp.CertPEM + sp.KeyPEM))
	base := certsDir + "/" + sp.ID + "-" + hex.EncodeToString(sum[:6])
	return base + ".crt", base + ".key"
}

// writeCert writes a site's uploaded certificate and removes its earlier ones.
func (a *applier) writeCert(ctx context.Context, sp agentproto.SiteSpec) error {
	if sp.TLS != "custom" {
		a.removeCerts(sp.ID, "")
		return nil
	}
	dir := a.sys.path(certsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if _, err := a.sys.run(ctx, "chown", "root:caddy", dir); err != nil {
		return err
	}
	cert, key := certPaths(sp)
	if err := os.WriteFile(a.sys.path(cert), []byte(strings.TrimSpace(sp.CertPEM)+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(a.sys.path(key), []byte(strings.TrimSpace(sp.KeyPEM)+"\n"), 0o640); err != nil {
		return err
	}
	if _, err := a.sys.run(ctx, "chown", "root:caddy", a.sys.path(key)); err != nil {
		return err
	}
	a.removeCerts(sp.ID, strings.TrimSuffix(filepath.Base(cert), ".crt"))
	return nil
}

// removeCerts deletes a site's certificate files, except those named keep.
func (a *applier) removeCerts(siteID, keep string) {
	files, _ := filepath.Glob(a.sys.path(certsDir + "/" + siteID + "-*"))
	for _, f := range files {
		name := filepath.Base(f)
		if keep == "" || (name != keep+".crt" && name != keep+".key") {
			os.Remove(f)
		}
	}
}
