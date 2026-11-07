package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"ekpanel/internal/agentproto"
)

// Railpack builds apps that have no Dockerfile: it detects the language and framework and
// writes a build plan, which BuildKit builds through Railpack's frontend image. The agent
// downloads one pinned version and checks it against the checksums published with it.

const (
	railpackVersion  = "0.43.0"
	railpackFrontend = "ghcr.io/railwayapp/railpack-frontend:v" + railpackVersion
	railpackDir      = "/usr/local/lib/ekpanel"
)

// railpackSHA256 is from the release's checksums.txt, per Go architecture.
var railpackSHA256 = map[string]string{
	"amd64": "248a8f60f36854aabed6124113631078a802de3461660855fc4de9c26065c1fb",
	"arm64": "732ece14eb4f4676a43ebba19431f954edf7b130acb76159ef0f2ff86ac42ac9",
}

var railpackArch = map[string]string{"amd64": "x86_64", "arm64": "arm64"}

// ensureRailpack returns the path of the pinned Railpack, downloading it the first time.
func ensureRailpack(ctx context.Context, sys *system, w *streamWriter) (string, error) {
	bin := sys.path(railpackDir + "/railpack-" + railpackVersion)
	if info, err := os.Stat(bin); err == nil && info.Mode().IsRegular() {
		return bin, nil
	}
	sum, arch := railpackSHA256[runtime.GOARCH], railpackArch[runtime.GOARCH]
	if sum == "" {
		return "", fmt.Errorf("Railpack is not available for %s", runtime.GOARCH)
	}
	w.step("Downloading Railpack %s", railpackVersion)
	url := fmt.Sprintf("https://github.com/railwayapp/railpack/releases/download/v%[1]s/railpack-v%[1]s-%[2]s-unknown-linux-musl.tar.gz", railpackVersion, arch)
	archive, err := sys.fetch(ctx, url)
	if err != nil {
		return "", fmt.Errorf("download Railpack: %w", err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != sum {
		return "", errors.New("the Railpack download does not match its published checksum")
	}
	program, err := untarFile(archive, "railpack")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		return "", err
	}
	tmp := bin + ".tmp"
	if err := os.WriteFile(tmp, program, 0o755); err != nil {
		return "", err
	}
	return bin, os.Rename(tmp, bin)
}

// untarFile returns the regular file called name (at any depth) from a .tar.gz.
func untarFile(archive []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == name {
			return io.ReadAll(io.LimitReader(tr, 256<<20))
		}
	}
}

// buildSecretsHash changes whenever a variable does, so build steps that read them are
// not taken from the cache with old values.
func buildSecretsHash(env []agentproto.EnvVar) string {
	lines := make([]string, 0, len(env))
	for _, e := range env {
		lines = append(lines, e.Key+"="+e.Value)
	}
	slices.Sort(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// railpackBuild plans and builds the image of the folder dir. Variables reach Railpack
// and the build as names only; their values come from the environment.
func railpackBuild(ctx context.Context, sys *system, p agentproto.BuildParams, w *streamWriter, work, dir string, env []string) error {
	bin, err := ensureRailpack(ctx, sys, w)
	if err != nil {
		return err
	}
	plan, info := filepath.Join(work, "railpack-plan.json"), filepath.Join(work, "railpack-info.json")
	w.step("Planning the build with Railpack %s", railpackVersion)
	args := []string{"prepare", dir, "--plan-out", plan, "--info-out", info}
	for _, e := range p.Env {
		args = append(args, "--env", e.Key)
	}
	if _, err := sys.exec(ctx, command{name: bin, args: args, env: env, stdout: w, stderr: w}); err != nil {
		return fmt.Errorf("Railpack could not plan a build for this repository: %w", err)
	}
	w.step("Building with Railpack")
	args = []string{"build", "--progress=plain", "--build-arg", "BUILDKIT_SYNTAX=" + railpackFrontend, "--file", plan,
		"--build-arg", "secrets-hash=" + buildSecretsHash(p.Env), "--build-arg", "cache-key=" + p.App,
		"--tag", p.Image, "--label", "ekpanel.app=" + p.App}
	for _, e := range p.Env {
		args = append(args, "--secret", "id="+e.Key+",env="+e.Key)
	}
	if p.Owner != "" {
		args = append(args, "--cgroup-parent", customerSlice(ownerKey(p.Owner)))
	}
	args = append(args, dir)
	if _, err := sys.exec(ctx, command{name: "docker", args: args, env: env, stdout: w, stderr: w}); err != nil {
		return fmt.Errorf("the build failed: %w", err)
	}
	return nil
}
