package agent

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"ekpanel/internal/agentproto"
)

// Importing one website from an archive of its files (any host; aaPanel's site backups)
// and, if it has one, a database dump.

// importList lists the files in the import folder that an import can use.
func importList(_ context.Context, s *system, _ struct{}) (any, *agentproto.OpError) {
	res := agentproto.ImportListResult{Files: []agentproto.ImportListedFile{}}
	entries, err := os.ReadDir(s.path(agentproto.ImportDir))
	if errors.Is(err, fs.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	for _, e := range entries {
		kind := agentproto.ImportFileKind(e.Name())
		info, err := e.Info()
		if kind == "" || err != nil || !info.Mode().IsRegular() {
			continue
		}
		res.Files = append(res.Files, agentproto.ImportListedFile{Name: e.Name(), Kind: kind, Bytes: info.Size(), Modified: info.ModTime()})
	}
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].Modified.After(res.Files[j].Modified) })
	return res, nil
}

// archiveEntry is one file, folder or link of a tar or zip archive, its name cleaned.
type archiveEntry struct {
	name     string
	kind     byte // tar type flag
	mode     int64
	size     int64
	linkname string
}

// walkArchive calls fn for every entry of a .tar(.gz) or .zip in the import folder.
func walkArchive(s *system, file string, fn func(e archiveEntry, r io.Reader) error) error {
	kind := agentproto.ImportFileKind(file)
	full := s.path(filepath.Join(agentproto.ImportDir, file))
	if _, err := os.Stat(full); err != nil {
		return errNoImportFile
	}
	clean := func(name string) string { return strings.TrimPrefix(path.Clean("/"+name), "/") }
	switch kind {
	case agentproto.ImportKindArchive:
		f, tr, err := openArchive(s, file)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			h, err := tr.Next()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return fmt.Errorf("read the archive: %w", err)
			}
			e := archiveEntry{name: clean(h.Name), kind: h.Typeflag, mode: h.Mode, size: h.Size, linkname: h.Linkname}
			if e.kind == tar.TypeRegA {
				e.kind = tar.TypeReg
			}
			if e.name == "" {
				continue
			}
			if err := fn(e, tr); err != nil {
				return err
			}
		}
	case agentproto.ImportKindZip:
		zr, err := zip.OpenReader(full)
		if err != nil {
			return fmt.Errorf("read the archive: %w", err)
		}
		defer zr.Close()
		for _, f := range zr.File {
			e := archiveEntry{name: clean(f.Name), kind: tar.TypeReg, mode: int64(f.Mode().Perm()), size: int64(f.UncompressedSize64)}
			switch {
			case e.name == "":
				continue
			case f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "/"):
				e.kind = tar.TypeDir
			case f.Mode()&fs.ModeSymlink != 0:
				e.kind = tar.TypeSymlink
			}
			if e.mode == 0 {
				e.mode = 0o644 // archives made on Windows keep no modes
			}
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("read %s: %w", e.name, err)
			}
			if e.kind == tar.TypeSymlink {
				b, _ := io.ReadAll(io.LimitReader(rc, 4096))
				e.linkname = string(b)
			}
			err = fn(e, rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
		return nil
	}
	return agentproto.ErrInvalidImportFile
}

var (
	aaPanelSiteRE    = regexp.MustCompile(`^web_(.+?)_\d{8}_\d{6}(?:_[A-Za-z0-9]+)?\.(?:tar\.gz|tgz|zip)$`)
	wpConfigDefineRE = regexp.MustCompile(`define\(\s*['"](DB_NAME|DB_USER|DB_PASSWORD|DB_HOST)['"]\s*,\s*('(?:[^'\\]|\\.)*'|"(?:[^"\\]|\\.)*")\s*\)`)
	wpPrefixRE       = regexp.MustCompile(`\$table_prefix\s*=\s*['"]([A-Za-z0-9_]+)['"]`)
)

func isDump(name string) bool {
	return strings.HasSuffix(name, ".sql") || strings.HasSuffix(name, ".sql.gz")
}

func importSiteInspect(_ context.Context, s *system, p agentproto.ImportSiteInspectParams) (any, *agentproto.OpError) {
	if k := agentproto.ImportFileKind(p.Archive); k != agentproto.ImportKindArchive && k != agentproto.ImportKindZip {
		return nil, opError(agentproto.ErrInvalidParams, "%v", agentproto.ErrInvalidImportFile)
	}
	type dirInfo struct{ wpConfig, wpIncludes, indexPHP, indexHTML, php bool }
	dirs := map[string]*dirInfo{}
	dir := func(d string) *dirInfo {
		if dirs[d] == nil {
			dirs[d] = &dirInfo{}
		}
		return dirs[d]
	}
	var files []archFile
	wpConfigs := map[string]string{}
	var plan agentproto.SitePlan
	plan.Dumps = []agentproto.ArchivedDump{}
	err := walkArchive(s, p.Archive, func(e archiveEntry, r io.Reader) error {
		d, base := path.Dir(e.name), path.Base(e.name)
		if d == "." {
			d = ""
		}
		if e.kind == tar.TypeDir {
			if base == "wp-includes" {
				pd := path.Dir(e.name)
				if pd == "." {
					pd = ""
				}
				dir(pd).wpIncludes = true
			}
			return nil
		}
		if e.kind != tar.TypeReg {
			files = append(files, archFile{e.name, 0})
			return nil
		}
		files = append(files, archFile{e.name, e.size})
		if isDump(e.name) {
			plan.Dumps = append(plan.Dumps, agentproto.ArchivedDump{Name: e.name, Bytes: e.size})
		}
		if strings.Contains(e.name, "/wp-includes/") || strings.HasPrefix(e.name, "wp-includes/") {
			pd := e.name[:strings.Index("/"+e.name, "/wp-includes/")]
			dir(strings.TrimSuffix(pd, "/")).wpIncludes = true
		}
		switch base {
		case "wp-config.php":
			dir(d).wpConfig = true
			if e.size < 1<<20 {
				b, _ := io.ReadAll(io.LimitReader(r, 1<<20))
				wpConfigs[d] = string(b)
			}
		case "index.php":
			dir(d).indexPHP = true
		case "index.html", "index.htm":
			dir(d).indexHTML = true
		}
		if strings.HasSuffix(base, ".php") {
			dir(d).php = true
		}
		return nil
	})
	if err != nil {
		return nil, archiveError(err)
	}
	// The website is the shallowest folder with wp-config.php, else with an index page.
	depth := func(d string) int {
		if d == "" {
			return 0
		}
		return strings.Count(d, "/") + 1
	}
	best := func(ok func(*dirInfo) bool) (string, bool) {
		found, root := false, ""
		for d, info := range dirs {
			if ok(info) && (!found || depth(d) < depth(root) || depth(d) == depth(root) && d < root) {
				found, root = true, d
			}
		}
		return root, found
	}
	root, ok := best(func(i *dirInfo) bool { return i.wpConfig })
	if !ok {
		if root, ok = best(func(i *dirInfo) bool { return i.indexPHP || i.indexHTML }); !ok {
			root = commonTop(files)
		}
	}
	plan.Root = root
	php := false
	for _, f := range files {
		if root == "" || strings.HasPrefix(f.name, root+"/") {
			plan.Files++
			plan.Bytes += f.size
			php = php || strings.HasSuffix(f.name, ".php")
		}
	}
	if len(files) == 0 {
		return nil, opError(agentproto.ErrNotCPanel, "the archive is empty")
	}
	switch {
	case dirs[root] != nil && dirs[root].wpConfig:
		plan.Kind = "wordpress"
		plan.WordPress = parseWPConfig(wpConfigs[root])
	case php:
		plan.Kind = "php"
	default:
		plan.Kind = "static"
	}
	if m := aaPanelSiteRE.FindStringSubmatch(p.Archive); m != nil && agentproto.ValidMailHost(strings.ToLower(m[1])) {
		plan.Domain = strings.ToLower(m[1])
	} else if first, _, _ := strings.Cut(root, "/"); first != "" && agentproto.ValidMailHost(strings.ToLower(path.Base(root))) {
		plan.Domain = strings.ToLower(path.Base(root))
	} else if name := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(p.Archive, ".zip"), ".gz"), ".tar"), ".tgz")); agentproto.ValidMailHost(name) {
		plan.Domain = name // example.com.zip
	}
	return plan, nil
}

type archFile struct {
	name string
	size int64
}

// commonTop is the one top folder every entry is in, or "".
func commonTop(files []archFile) string {
	top := ""
	for _, f := range files {
		first, _, ok := strings.Cut(f.name, "/")
		if !ok || top != "" && first != top {
			return ""
		}
		top = first
	}
	return top
}

// parseWPConfig reads the database settings of wp-config.php; the password becomes its
// MySQL native hash, so the database can be made with it.
func parseWPConfig(text string) *agentproto.WordPressSettings {
	wp := &agentproto.WordPressSettings{}
	for _, m := range wpConfigDefineRE.FindAllStringSubmatch(text, -1) {
		v := m[2][1 : len(m[2])-1]
		v = strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`).Replace(v)
		switch m[1] {
		case "DB_NAME":
			wp.DBName = v
		case "DB_USER":
			wp.DBUser = v
		case "DB_HOST":
			wp.DBHost = v
		case "DB_PASSWORD":
			if v != "" {
				wp.PasswordHash = mysqlNativeHash(v)
			}
		}
	}
	if m := wpPrefixRE.FindStringSubmatch(text); m != nil {
		wp.TablePrefix = m[1]
	}
	return wp
}

// mysqlNativeHash is MySQL's mysql_native_password hash: "*" and SHA1(SHA1(password)).
func mysqlNativeHash(pw string) string {
	a := sha1.Sum([]byte(pw))
	b := sha1.Sum(a[:])
	return "*" + strings.ToUpper(hex.EncodeToString(b[:]))
}

func importSiteApply(ctx context.Context, s *system, p agentproto.ImportSiteApplyParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	var res agentproto.ImportApplyResult
	w := newHomeWriter(s, &res, nil)
	defer w.close()
	// The website's fresh public_html (the panel's placeholder page) gives way to the copy.
	os.Remove(s.path(path.Join("/home", p.User, "public_html", "index.html")))
	load := func(r io.Reader, name string) error {
		if strings.HasSuffix(name, ".gz") {
			zr, err := gzip.NewReader(bufio.NewReaderSize(r, 1<<20))
			if err != nil {
				return fmt.Errorf("read %s: %w", name, err)
			}
			defer zr.Close()
			r = zr
		}
		if err := s.loadDumpFrom(ctx, r, *p.Database); err != nil {
			return err
		}
		res.Databases = append(res.Databases, p.Database.Name)
		return nil
	}
	err := walkArchive(s, p.Archive, func(e archiveEntry, r io.Reader) error {
		if p.DumpInArchive != "" && e.name == p.DumpInArchive {
			return load(r, e.name) // a dump is loaded, never published
		}
		rel, ok := e.name, true
		if p.Root != "" {
			rel, ok = strings.CutPrefix(e.name, p.Root+"/")
		}
		if !ok {
			if e.kind != tar.TypeDir && e.name != p.Root {
				w.skipped["outside_website"]++
			}
			return nil
		}
		dest := path.Join("/home", p.User, "public_html", rel)
		// aaPanel locks each website to its old folder with open_basedir in .user.ini; here
		// PHP's pool does that, and the old path would stop the website.
		if rel == ".user.ini" && e.kind == tar.TypeReg && e.size < 1<<20 {
			b, err := io.ReadAll(r)
			if err != nil {
				return err
			}
			var keep []string
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "open_basedir") {
					w.skipped["open_basedir"]++
					continue
				}
				keep = append(keep, l)
			}
			nb := []byte(strings.Join(keep, "\n"))
			return w.put(p.User, dest, rel, e.kind, e.mode, int64(len(nb)), e.linkname, bytes.NewReader(nb))
		}
		return w.put(p.User, dest, rel, e.kind, e.mode, e.size, e.linkname, r)
	})
	if err != nil {
		return nil, archiveError(err)
	}
	if p.Dump != "" {
		f, err := os.Open(s.path(filepath.Join(agentproto.ImportDir, p.Dump)))
		if err != nil {
			return nil, archiveError(errNoImportFile)
		}
		err = load(f, p.Dump)
		f.Close()
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
	}
	if p.WordPress && p.Database != nil {
		if err := fixWPConfig(w, p.User, *p.Database); err != nil {
			return nil, opError(agentproto.ErrFailed, "wp-config.php: %v", err)
		}
	}
	if err := w.finish(ctx); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return res, nil
}

// fixWPConfig points wp-config.php at the database here (its name and user, on localhost);
// the password stays, as the database was made with its hash.
func fixWPConfig(w *homeWriter, user string, db agentproto.TransferDatabase) error {
	root, err := w.rootOf(user)
	if err != nil {
		return err
	}
	b, err := root.ReadFile("public_html/wp-config.php")
	if err != nil {
		return err
	}
	set := map[string]string{"DB_NAME": db.Name, "DB_USER": db.User, "DB_HOST": "localhost"}
	out := wpConfigDefineRE.ReplaceAllStringFunc(string(b), func(m string) string {
		key := wpConfigDefineRE.FindStringSubmatch(m)[1]
		if v, ok := set[key]; ok {
			return "define( '" + key + "', '" + v + "' )"
		}
		return m
	})
	if out == string(b) {
		return nil
	}
	info, err := root.Stat("public_html/wp-config.php")
	if err != nil {
		return err
	}
	return root.WriteFile("public_html/wp-config.php", []byte(out), info.Mode().Perm())
}
