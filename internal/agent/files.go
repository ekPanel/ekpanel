package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ekpanel/internal/agentproto"
)

// Limits that keep one archive from filling the disk or the inode table.
const (
	maxExtractBytes   = 10 << 30
	maxExtractEntries = 200_000
)

// trashDir holds deleted files until the trash is emptied.
const trashDir = "/.trash"

// ---- Parent side: start a worker for one request ----

// workerRequest is what the agent sends to its file worker process on stdin.
type workerRequest struct {
	Home   string                 `json:"home"`
	UID    int                    `json:"uid"`
	GID    int                    `json:"gid"`
	Params agentproto.FilesParams `json:"params"`
}

type workerResponse struct {
	Result *agentproto.FilesResult `json:"result,omitempty"`
	Error  *agentproto.OpError     `json:"error,omitempty"`
}

// runFiles checks the site account and runs the action in a separate worker process
// (`ekpanel-agent files`), which locks itself into the account's home folder.
func runFiles(ctx context.Context, sys *system, p agentproto.FilesParams) (*agentproto.FilesResult, *agentproto.OpError) {
	if !userRE.MatchString(p.User) || slices.Contains(reserved, p.User) {
		return nil, opError(agentproto.ErrInvalidParams, "invalid site account")
	}
	out, err := sys.run(ctx, "id", "-nG", p.User)
	if err != nil || !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return nil, opError(agentproto.ErrNotAllowed, "%q is not an ekPanel site account", p.User)
	}
	u, err := user.Lookup(p.User)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	req, _ := json.Marshal(workerRequest{Home: "/home/" + p.User, UID: uid, GID: gid, Params: p})

	self, err := os.Executable()
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	cmd := exec.CommandContext(ctx, self, "files")
	cmd.Stdin = strings.NewReader(string(req))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	raw, err := cmd.Output()
	var resp workerResponse
	if jerr := json.Unmarshal(raw, &resp); jerr != nil {
		if err == nil {
			err = jerr
		}
		return nil, opError(agentproto.ErrFailed, "file worker: %v", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// ---- Worker side ----

// RunFileWorker is the entry point of `ekpanel-agent files`. It starts as root, locks
// itself into the site's home with chroot, drops to the site's account, performs one
// action and writes the result as JSON.
func RunFileWorker(in io.Reader, out io.Writer) {
	enc := json.NewEncoder(out)
	var req workerRequest
	if err := json.NewDecoder(io.LimitReader(in, 4<<20)).Decode(&req); err != nil {
		enc.Encode(workerResponse{Error: opError(agentproto.ErrInvalidParams, "%v", err)})
		return
	}
	if req.UID < 1000 || req.GID < 1000 {
		enc.Encode(workerResponse{Error: opError(agentproto.ErrNotAllowed, "refusing to run as a system account")})
		return
	}
	// Order matters: chroot needs root, and after Setuid nothing can undo it.
	steps := []func() error{
		func() error { return syscall.Chroot(req.Home) },
		func() error { return os.Chdir("/") },
		func() error { return syscall.Setgroups([]int{req.GID}) },
		func() error { return syscall.Setgid(req.GID) },
		func() error { return syscall.Setuid(req.UID) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			enc.Encode(workerResponse{Error: opError(agentproto.ErrFailed, "lock into site folder: %v", err)})
			return
		}
	}
	syscall.Umask(0o022)
	res, opErr := (&fileWorker{root: "/"}).do(req.Params)
	enc.Encode(workerResponse{Result: res, Error: opErr})
}

// fileWorker performs actions on paths below root. In production root is "/" inside the
// chroot; tests use a temporary folder.
type fileWorker struct{ root string }

// resolve turns a site-relative path ("/public_html/a.txt") into a real one below root.
func (w *fileWorker) resolve(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", errors.New("empty or invalid path")
	}
	clean := path.Clean("/" + p)
	return filepath.Join(w.root, filepath.FromSlash(clean)), nil
}

func fileOpError(err error) *agentproto.OpError {
	var opErr *agentproto.OpError
	switch {
	case errors.As(err, &opErr):
		return opErr
	case errors.Is(err, fs.ErrNotExist):
		return opError("not_found", "%v", trimRoot(err))
	case errors.Is(err, fs.ErrExist):
		return opError("exists", "%v", trimRoot(err))
	case errors.Is(err, fs.ErrPermission):
		return opError(agentproto.ErrNotAllowed, "%v", trimRoot(err))
	default:
		return opError(agentproto.ErrFailed, "%v", trimRoot(err))
	}
}

// trimRoot keeps messages free of the worker's internal paths.
func trimRoot(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Op + ": " + pe.Err.Error()
	}
	return err.Error()
}

func (w *fileWorker) do(p agentproto.FilesParams) (*agentproto.FilesResult, *agentproto.OpError) {
	var res *agentproto.FilesResult
	var err error
	switch p.Action {
	case "list":
		res, err = w.list(p.Path)
	case "read":
		res, err = w.read(p.Path)
	case "write":
		res, err = w.write(p.Path, p.Data)
	case "mkdir":
		res, err = w.mkdir(p.Path)
	case "rename":
		res, err = w.rename(p.Path, p.To, p.Replace)
	case "delete":
		res, err = w.delete(p.Paths)
	case "empty_trash":
		res, err = w.emptyTrash()
	case "chmod":
		res, err = w.chmod(p.Path, p.Mode)
	case "extract":
		res, err = w.extract(p.Path, p.To)
	case "compress":
		res, err = w.compress(p.Paths, p.To)
	case "read_chunk":
		res, err = w.readChunk(p.Path, p.Offset)
	case "write_chunk":
		res, err = w.writeChunk(p.Path, p.Offset, p.Data, p.Final, p.Replace)
	default:
		return nil, opError(agentproto.ErrInvalidParams, "unknown file action %q", p.Action)
	}
	if err != nil {
		return nil, fileOpError(err)
	}
	return res, nil
}

func entryFor(name string, info fs.FileInfo, full string) agentproto.FileEntry {
	e := agentproto.FileEntry{Name: name, Size: info.Size(), Mode: fmt.Sprintf("%03o", info.Mode().Perm()), Modified: info.ModTime().UTC()}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		e.Type = "link"
		e.Target, _ = os.Readlink(full)
	case info.IsDir():
		e.Type = "dir"
	default:
		e.Type = "file"
	}
	return e
}

func (w *fileWorker) list(p string) (*agentproto.FilesResult, error) {
	dir, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	res := &agentproto.FilesResult{Entries: []agentproto.FileEntry{}}
	for _, it := range items {
		info, err := os.Lstat(filepath.Join(dir, it.Name()))
		if err != nil {
			continue
		}
		res.Entries = append(res.Entries, entryFor(it.Name(), info, filepath.Join(dir, it.Name())))
	}
	sort.Slice(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if (a.Type == "dir") != (b.Type == "dir") {
			return a.Type == "dir"
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	return res, nil
}

func (w *fileWorker) read(p string) (*agentproto.FilesResult, error) {
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, opError(agentproto.ErrInvalidParams, "not a regular file")
	}
	if info.Size() > agentproto.FileEditLimit {
		return nil, opError("too_large", "file is larger than the editor limit")
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return nil, err
	}
	return &agentproto.FilesResult{Data: data, Size: info.Size()}, nil
}

// writeAtomic writes through a temporary file, keeping the old file's permissions.
func writeAtomic(full string, data []byte) error {
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(full); err == nil {
		if !info.Mode().IsRegular() {
			return opError(agentproto.ErrInvalidParams, "not a regular file")
		}
		mode = info.Mode().Perm()
	}
	tmp := full + ".ekpanel-save"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (w *fileWorker) write(p string, data []byte) (*agentproto.FilesResult, error) {
	if len(data) > agentproto.FileEditLimit {
		return nil, opError("too_large", "file is larger than the editor limit")
	}
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	return &agentproto.FilesResult{Size: int64(len(data))}, writeAtomic(full, data)
}

func (w *fileWorker) mkdir(p string) (*agentproto.FilesResult, error) {
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	return &agentproto.FilesResult{}, os.Mkdir(full, 0o755)
}

func (w *fileWorker) rename(from, to string, replace bool) (*agentproto.FilesResult, error) {
	src, err := w.resolve(from)
	if err != nil {
		return nil, err
	}
	dst, err := w.resolve(to)
	if err != nil {
		return nil, err
	}
	if src == w.root || src == filepath.Join(w.root, "public_html") {
		return nil, opError(agentproto.ErrNotAllowed, "this folder cannot be moved")
	}
	if !replace {
		if _, err := os.Lstat(dst); err == nil {
			return nil, opError("exists", "something with that name already exists")
		}
	}
	return &agentproto.FilesResult{Path: to}, os.Rename(src, dst)
}

// delete moves items into the trash; items already in the trash are deleted for good.
func (w *fileWorker) delete(paths []string) (*agentproto.FilesResult, error) {
	trash := filepath.Join(w.root, trashDir)
	stamp := time.Now().UTC().Format("20060102-150405")
	res := &agentproto.FilesResult{}
	for i, p := range paths {
		full, err := w.resolve(p)
		if err != nil {
			return nil, err
		}
		switch full {
		case w.root, trash, filepath.Join(w.root, "public_html"), filepath.Join(w.root, "logs"), filepath.Join(w.root, "tmp"):
			return nil, opError(agentproto.ErrNotAllowed, "this folder cannot be deleted")
		}
		if _, err := os.Lstat(full); err != nil {
			return nil, err
		}
		if strings.HasPrefix(full, trash+string(filepath.Separator)) {
			if err := os.RemoveAll(full); err != nil {
				return nil, err
			}
		} else {
			name := fmt.Sprintf("%s-%d-%s", stamp, i, filepath.Base(full))
			if err := os.Rename(full, filepath.Join(trash, name)); err != nil {
				return nil, err
			}
			res.Path = trashDir
		}
		res.Count++
	}
	return res, nil
}

func (w *fileWorker) emptyTrash() (*agentproto.FilesResult, error) {
	trash := filepath.Join(w.root, trashDir)
	items, err := os.ReadDir(trash)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if err := os.RemoveAll(filepath.Join(trash, it.Name())); err != nil {
			return nil, err
		}
	}
	return &agentproto.FilesResult{Count: len(items)}, nil
}

func (w *fileWorker) chmod(p, mode string) (*agentproto.FilesResult, error) {
	m, err := strconv.ParseUint(mode, 8, 32)
	if err != nil || m > 0o777 {
		return nil, opError(agentproto.ErrInvalidParams, "permissions must be three octal digits, such as 644")
	}
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, opError(agentproto.ErrInvalidParams, "cannot change permissions of a link")
	}
	return &agentproto.FilesResult{}, os.Chmod(full, fs.FileMode(m))
}

// ---- Uploads and downloads in pieces ----

func uploadTemp(full string) string {
	return filepath.Join(filepath.Dir(full), "."+filepath.Base(full)+".ekpanel-upload")
}

func (w *fileWorker) writeChunk(p string, offset int64, data []byte, final, replace bool) (*agentproto.FilesResult, error) {
	if len(data) > agentproto.FileChunkSize {
		return nil, opError("too_large", "chunk too large")
	}
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	tmp := uploadTemp(full)
	flags := os.O_WRONLY | os.O_CREATE
	if offset == 0 {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.Size() != offset {
		f.Close()
		// The client resumes from Size.
		return &agentproto.FilesResult{Size: info.Size()}, opError("bad_offset", "upload is at %d bytes, not %d", info.Size(), offset)
	}
	if _, err := f.WriteAt(data, offset); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	size := offset + int64(len(data))
	if final {
		if !replace {
			if _, err := os.Lstat(full); err == nil {
				os.Remove(tmp)
				return nil, opError("exists", "a file with that name already exists")
			}
		}
		if err := os.Rename(tmp, full); err != nil {
			return nil, err
		}
	}
	return &agentproto.FilesResult{Size: size, Path: p}, nil
}

func (w *fileWorker) readChunk(p string, offset int64) (*agentproto.FilesResult, error) {
	full, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, opError(agentproto.ErrInvalidParams, "not a regular file")
	}
	buf := make([]byte, agentproto.FileChunkSize)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &agentproto.FilesResult{Data: buf[:n], Size: info.Size(), EOF: offset+int64(n) >= info.Size()}, nil
}

// ---- Archives ----

// safeTarget joins an archive entry name to dest, refusing names that would land outside it
// ("zip slip": ../../etc/passwd or absolute paths).
func safeTarget(dest, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}
	target := filepath.Join(dest, filepath.FromSlash(name))
	if target != dest && !strings.HasPrefix(target, dest+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q points outside the folder", name)
	}
	return target, nil
}

type extractor struct {
	dest    string
	written int64
	count   int
}

func (x *extractor) file(name string, mode fs.FileMode, r io.Reader) error {
	x.count++
	if x.count > maxExtractEntries {
		return opError("too_large", "archive has too many entries")
	}
	target, err := safeTarget(x.dest, name)
	if err != nil {
		return opError(agentproto.ErrNotAllowed, "%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	// Archives often carry no permissions (0000) or private ones; web files must be readable
	// by the web server, so only the executable bit is taken from the archive.
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// An existing file keeps its old mode on open; set it explicitly.
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxExtractBytes-x.written+1))
	f.Close()
	x.written += n
	if x.written > maxExtractBytes {
		return opError("too_large", "archive expands to more than 10 GB")
	}
	return err
}

func (x *extractor) dir(name string) error {
	target, err := safeTarget(x.dest, name)
	if err != nil {
		return opError(agentproto.ErrNotAllowed, "%v", err)
	}
	return os.MkdirAll(target, 0o755)
}

// extract unpacks a .zip, .tar, .tar.gz or .tgz archive into the folder `to` (default:
// the archive's folder). Links inside archives are skipped, never created.
func (w *fileWorker) extract(p, to string) (*agentproto.FilesResult, error) {
	src, err := w.resolve(p)
	if err != nil {
		return nil, err
	}
	if to == "" {
		to = path.Dir(path.Clean("/" + p))
	}
	dest, err := w.resolve(to)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}
	x := &extractor{dest: dest}
	lower := strings.ToLower(src)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		zr, err := zip.OpenReader(src)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		for _, f := range zr.File {
			switch {
			case f.Mode()&fs.ModeSymlink != 0:
				continue
			case f.FileInfo().IsDir():
				err = x.dir(f.Name)
			default:
				var rc io.ReadCloser
				if rc, err = f.Open(); err == nil {
					err = x.file(f.Name, f.Mode(), rc)
					rc.Close()
				}
			}
			if err != nil {
				return nil, err
			}
		}
	case strings.HasSuffix(lower, ".tar"), strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		f, err := os.Open(src)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var r io.Reader = f
		if !strings.HasSuffix(lower, ".tar") {
			gz, err := gzip.NewReader(f)
			if err != nil {
				return nil, err
			}
			defer gz.Close()
			r = gz
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, err
			}
			switch h.Typeflag {
			case tar.TypeDir:
				err = x.dir(h.Name)
			case tar.TypeReg:
				err = x.file(h.Name, fs.FileMode(h.Mode), tr)
			default:
				continue // links, devices and the like are skipped
			}
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, opError(agentproto.ErrInvalidParams, "only .zip, .tar, .tar.gz and .tgz archives can be extracted")
	}
	return &agentproto.FilesResult{Count: x.count, Path: to}, nil
}

// compress puts files and folders into a new .zip archive at `to`.
func (w *fileWorker) compress(paths []string, to string) (*agentproto.FilesResult, error) {
	if !strings.HasSuffix(strings.ToLower(to), ".zip") {
		return nil, opError(agentproto.ErrInvalidParams, "the archive name must end in .zip")
	}
	dst, err := w.resolve(to)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil, opError("exists", "an archive with that name already exists")
	}
	tmp := dst + ".ekpanel-save"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(out)
	count := 0
	for _, p := range paths {
		src, err := w.resolve(p)
		if err != nil {
			return nil, err
		}
		base := filepath.Dir(src)
		err = filepath.WalkDir(src, func(fp string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if fp == tmp || d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			rel, _ := filepath.Rel(base, fp)
			info, err := d.Info()
			if err != nil {
				return err
			}
			h, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			h.Name = filepath.ToSlash(rel)
			if d.IsDir() {
				h.Name += "/"
				_, err = zw.CreateHeader(h)
				return err
			}
			h.Method = zip.Deflate
			fw, err := zw.CreateHeader(h)
			if err != nil {
				return err
			}
			f, err := os.Open(fp)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(fw, f)
			count++
			return err
		})
		if err != nil {
			zw.Close()
			out.Close()
			os.Remove(tmp)
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		out.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := out.Close(); err != nil {
		return nil, err
	}
	return &agentproto.FilesResult{Count: count, Path: to}, os.Rename(tmp, dst)
}
