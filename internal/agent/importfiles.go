package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"ekpanel/internal/agentproto"
)

// homeWriter puts imported files into websites' home folders. Every write goes through the
// home folder's os.Root: links in an archive (each checked alone, but chained) cannot lead
// a root-owned write out of the website's home.
type homeWriter struct {
	s       *system
	res     *agentproto.ImportApplyResult
	paths   *homeRewriter // nil: files are written as they are
	roots   map[string]*os.Root
	skipped map[string]int
	touched map[string]bool // homes to give to their accounts afterwards
}

func newHomeWriter(s *system, res *agentproto.ImportApplyResult, paths *homeRewriter) *homeWriter {
	return &homeWriter{s: s, res: res, paths: paths, roots: map[string]*os.Root{}, skipped: map[string]int{}, touched: map[string]bool{}}
}

func (w *homeWriter) close() {
	for _, r := range w.roots {
		r.Close()
	}
}

func (w *homeWriter) rootOf(user string) (*os.Root, error) {
	if r, ok := w.roots[user]; ok {
		return r, nil
	}
	r, err := os.OpenRoot(w.s.path("/home/" + user))
	if err != nil {
		return nil, err
	}
	w.roots[user] = r
	return r, nil
}

// put writes one archive entry at dest, a path in /home/<user>/; rel is its name in the
// archive (for path rewriting). kind is a tar type flag.
func (w *homeWriter) put(user, dest, rel string, kind byte, mode, size int64, linkname string, r io.Reader) error {
	home := "/home/" + user
	name, ok := strings.CutPrefix(dest, home+"/")
	if !ok {
		return nil
	}
	root, err := w.rootOf(user)
	if err != nil {
		return err
	}
	// A path that leads out of the home (through links) is left out, not an error.
	escapes := func(err error) bool { return err != nil && strings.Contains(err.Error(), "escapes from parent") }
	outside := func(err error) (bool, error) {
		if escapes(err) {
			w.skipped["links_outside"]++
			return true, nil
		}
		return err != nil, err
	}
	switch kind {
	case tar.TypeDir:
		if stop, err := outside(root.MkdirAll(name, 0o750)); stop {
			return err
		}
	case tar.TypeReg:
		if stop, err := outside(root.MkdirAll(path.Dir(name), 0o750)); stop {
			return err
		}
		out, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(mode)&0o777)
		if stop, err := outside(err); stop {
			return err
		}
		if w.paths != nil && size <= rewriteMaxSize && rewritable(rel) {
			b, err := io.ReadAll(r)
			if err != nil {
				out.Close()
				return err
			}
			if nb, changed := w.paths.rewrite(b); changed {
				b = nb
				w.res.PathsChangedCount++
				if len(w.res.PathsChanged) < 50 {
					w.res.PathsChanged = append(w.res.PathsChanged, strings.TrimPrefix(dest, "/home/"))
				}
			}
			r = bytes.NewReader(b)
		}
		n, err := io.Copy(out, r)
		out.Close()
		if err != nil {
			return err
		}
		w.res.Files++
		w.res.Bytes += n
	case tar.TypeSymlink:
		// Only links that stay inside the website.
		target := path.Clean(path.Join(path.Dir(dest), linkname))
		if path.IsAbs(linkname) || (target != home && !strings.HasPrefix(target, home+"/")) {
			w.skipped["links_outside"]++
			return nil
		}
		if stop, err := outside(root.MkdirAll(path.Dir(name), 0o750)); stop {
			return err
		}
		root.Remove(name)
		if stop, err := outside(root.Symlink(linkname, name)); stop {
			return err
		}
	default:
		w.skipped["special_files"]++
		return nil
	}
	w.touched[user] = true
	return nil
}

// finish gives the written files to their accounts and lets the web server read
// public_html; it also reports what was left out.
func (w *homeWriter) finish(ctx context.Context) error {
	for user := range w.touched {
		home := w.s.path("/home/" + user)
		entries, _ := os.ReadDir(home)
		for _, e := range entries {
			// The home folder itself stays root's (SFTP); everything in it is the account's.
			if _, err := w.s.run(ctx, "chown", "-R", user+":"+user, filepath.Join(home, e.Name())); err != nil {
				return err
			}
		}
		if _, err := w.s.run(ctx, "setfacl", "-R", "-m", "u:caddy:rX,d:u:caddy:rX", filepath.Join(home, "public_html")); err != nil {
			return err
		}
	}
	for what, n := range w.skipped {
		w.res.Skipped = append(w.res.Skipped, agentproto.ImportSkip{Name: what, Count: n})
	}
	sort.Slice(w.res.Skipped, func(i, j int) bool { return w.res.Skipped[i].Name < w.res.Skipped[j].Name })
	return nil
}
