package agent

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"ekpanel/internal/agentproto"
)

// Importing a cPanel account from its backup archive (see agentproto.OpImportInspect).
//
// A cpmove archive has one top folder (cpmove-<user>/ or <user>/) with, among others:
// userdata/main and userdata/<domain> (the domains and their document roots), mysql.sql (the
// database users, their password hashes and grants), mysql/<database>.sql (dumps), cron/<user>,
// dnszones/<zone>.db, va/<domain> (mail forwarders), and the home folder, as homedir.tar or a
// homedir/ folder (with etc/<domain>/passwd listing mail accounts).

// homeSkip are top folders of a cPanel home that are cPanel's own or are imported elsewhere.
var homeSkip = []string{"mail", "etc", ".cpanel", ".trash", "tmp", "logs", "access-logs", "ssl", ".htpasswds",
	".cagefs", "public_ftp", ".spamassassin", ".cphorde", ".softaculous", ".sqmailattach", ".subaccounts", ".cl.selector"}

func openArchive(s *system, file string) (io.ReadCloser, *tar.Reader, error) {
	if err := agentproto.ValidImportFile(file); err != nil {
		return nil, nil, err
	}
	f, err := os.Open(s.path(filepath.Join(agentproto.ImportDir, file)))
	if err != nil {
		return nil, nil, fmt.Errorf("no archive %s in %s", file, agentproto.ImportDir)
	}
	var r io.Reader = bufio.NewReaderSize(f, 1<<20)
	if !strings.HasSuffix(file, ".tar") {
		zr, err := gzip.NewReader(r)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("not a .tar.gz archive: %w", err)
		}
		r = zr
	}
	return f, tar.NewReader(r), nil
}

// inArchive is a name below the archive's top folder ("userdata/main"), or "" for the top.
func inArchive(name string) string {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// homeEntries calls fn for every entry of the home folder, whether the archive holds it as
// homedir.tar or as a homedir/ folder; rel is relative to the home folder.
func homeEntries(tr *tar.Reader, h *tar.Header, rel string, fn func(rel string, h *tar.Header, r io.Reader) error) (bool, error) {
	switch {
	case rel == "homedir.tar":
		inner := tar.NewReader(tr)
		for {
			ih, err := inner.Next()
			if err == io.EOF {
				return true, nil
			}
			if err != nil {
				return true, fmt.Errorf("read homedir.tar: %w", err)
			}
			r := strings.TrimPrefix(path.Clean("/"+ih.Name), "/")
			if r == "" || r == "." {
				continue
			}
			if err := fn(r, ih, inner); err != nil {
				return true, err
			}
		}
	case strings.HasPrefix(rel, "homedir/"):
		return true, fn(strings.TrimPrefix(rel, "homedir/"), h, tr)
	}
	return false, nil
}

// ---- Inspect ----

func importInspect(_ context.Context, s *system, p agentproto.ImportInspectParams) (any, *agentproto.OpError) {
	f, tr, err := openArchive(s, p.File)
	if err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	defer f.Close()
	small := map[string]string{} // userdata/*, mysql.sql, cron/*, cp/*
	var plan agentproto.CPanelPlan
	dumps := map[string]int64{}
	mailDomains := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "read the archive: %v", err)
		}
		rel := inArchive(h.Name)
		handled, err := homeEntries(tr, h, rel, func(r string, ih *tar.Header, body io.Reader) error {
			if ih.Typeflag == tar.TypeReg {
				plan.HomeBytes += ih.Size
			}
			// etc/<domain>/passwd lists the domain's mail accounts.
			if parts := strings.Split(r, "/"); len(parts) == 3 && parts[0] == "etc" && parts[2] == "passwd" && ih.Size > 0 {
				mailDomains[parts[1]] = true
				plan.MailAccounts += int(countLines(body, ih.Size))
			}
			return nil
		})
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		if handled || h.Typeflag != tar.TypeReg {
			continue
		}
		switch {
		case strings.HasPrefix(rel, "userdata/") || rel == "mysql.sql" || strings.HasPrefix(rel, "cron/") || strings.HasPrefix(rel, "cp/"):
			if h.Size < 4<<20 {
				b, _ := io.ReadAll(io.LimitReader(tr, 4<<20))
				small[rel] = string(b)
			}
		case strings.HasPrefix(rel, "mysql/") && strings.HasSuffix(rel, ".sql"):
			dumps[strings.TrimSuffix(strings.TrimPrefix(rel, "mysql/"), ".sql")] += h.Size
		case strings.HasPrefix(rel, "dnszones/") && strings.HasSuffix(rel, ".db"):
			plan.DNSZones = append(plan.DNSZones, strings.TrimSuffix(strings.TrimPrefix(rel, "dnszones/"), ".db"))
		case strings.HasPrefix(rel, "va/") && h.Size > 0:
			mailDomains[strings.TrimPrefix(rel, "va/")] = true
		}
	}
	if err := planFromFiles(&plan, small, dumps); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	plan.MailDomains = len(mailDomains)
	sort.Strings(plan.DNSZones)
	return plan, nil
}

func countLines(r io.Reader, size int64) int64 {
	b, _ := io.ReadAll(io.LimitReader(r, min(size, 4<<20)))
	n := int64(0)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n
}

// yamlish reads cPanel's simple YAML: "key: value" pairs, and lists or maps under a key.
type yamlish struct {
	scalars map[string]string
	lists   map[string][]string
	maps    map[string]map[string]string
}

func parseYAMLish(s string) yamlish {
	y := yamlish{scalars: map[string]string{}, lists: map[string][]string{}, maps: map[string]map[string]string{}}
	key := ""
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") || line == "---" {
			continue
		}
		unq := func(v string) string { return strings.Trim(strings.TrimSpace(v), `'"`) }
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(k)
			if v = strings.TrimSpace(v); v != "" && v != "{}" && v != "[]" {
				y.scalars[key] = unq(v)
			}
			continue
		}
		t := strings.TrimSpace(line)
		if item, ok := strings.CutPrefix(t, "- "); ok {
			y.lists[key] = append(y.lists[key], unq(item))
		} else if k, v, ok := strings.Cut(t, ":"); ok {
			if y.maps[key] == nil {
				y.maps[key] = map[string]string{}
			}
			y.maps[key][unq(k)] = unq(v)
		}
	}
	return y
}

var (
	phpVersionRE  = regexp.MustCompile(`^(?:ea-|alt-)?php(\d)(\d)$`)
	grantRE       = regexp.MustCompile("(?i)GRANT .* ON `?([^`. ]+)`?\\.\\* TO '([^']+)'@'(?:localhost|%)'")
	nativeHashRE  = regexp.MustCompile(`(?i)'([^']+)'@'(?:localhost|%)' IDENTIFIED (?:WITH '?mysql_native_password'? AS|BY PASSWORD) '(\*[0-9A-F]{40})'`)
	cronEnvLineRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
)

func planFromFiles(plan *agentproto.CPanelPlan, small map[string]string, dumps map[string]int64) error {
	main, ok := small["userdata/main"]
	if !ok {
		return errors.New("this does not look like a cPanel backup: it has no userdata/main")
	}
	y := parseYAMLish(main)
	plan.MainDomain = y.scalars["main_domain"]
	for k, v := range small {
		if strings.HasPrefix(k, "cp/") {
			plan.User = strings.TrimPrefix(k, "cp/")
			for _, l := range strings.Split(v, "\n") {
				if u, ok := strings.CutPrefix(l, "USER="); ok {
					plan.User = strings.TrimSpace(u)
				}
			}
		}
	}
	domainFile := func(d string) yamlish { return parseYAMLish(small["userdata/"+d]) }
	docroot := func(y yamlish) string {
		dr := y.scalars["documentroot"]
		if i := strings.Index(dr, "/"+plan.User+"/"); plan.User != "" && i >= 0 {
			return dr[i+len(plan.User)+2:]
		}
		// /home/<user>/x: drop the first two parts.
		parts := strings.SplitN(strings.TrimPrefix(dr, "/"), "/", 3)
		if len(parts) == 3 {
			return parts[2]
		}
		return "public_html"
	}
	php := func(y yamlish) string {
		if m := phpVersionRE.FindStringSubmatch(y.scalars["phpversion"]); m != nil {
			return m[1] + "." + m[2]
		}
		return ""
	}
	if plan.MainDomain == "" {
		return errors.New("the backup names no main domain")
	}
	mainY := domainFile(plan.MainDomain)
	md := agentproto.CPanelDomain{Domain: plan.MainDomain, Kind: "main", DocRoot: docroot(mainY), PHPVersion: php(mainY),
		HasCert: small["userdata/"+plan.MainDomain+"_SSL"] != ""}
	md.Aliases = append(md.Aliases, y.lists["parked_domains"]...)
	plan.Domains = append(plan.Domains, md)
	addonSubs := map[string]bool{}
	addons := slices.Sorted(func(yield func(string) bool) {
		for d := range y.maps["addon_domains"] {
			if !yield(d) {
				return
			}
		}
	})
	for _, addon := range addons {
		sub := y.maps["addon_domains"][addon]
		addonSubs[sub] = true
		sy := domainFile(sub)
		plan.Domains = append(plan.Domains, agentproto.CPanelDomain{Domain: addon, Kind: "addon", DocRoot: docroot(sy), PHPVersion: php(sy),
			HasCert: small["userdata/"+sub+"_SSL"] != ""})
	}
	for _, sub := range y.lists["sub_domains"] {
		if addonSubs[sub] {
			continue
		}
		sy := domainFile(sub)
		plan.Domains = append(plan.Domains, agentproto.CPanelDomain{Domain: sub, Kind: "sub", DocRoot: docroot(sy), PHPVersion: php(sy),
			HasCert: small["userdata/"+sub+"_SSL"] != ""})
	}

	// Databases: their users and password hashes from mysql.sql.
	hashes := map[string]string{}
	dbUsers := map[string][]string{}
	for _, line := range strings.Split(small["mysql.sql"], "\n") {
		if m := nativeHashRE.FindStringSubmatch(line); m != nil {
			hashes[m[1]] = strings.ToUpper(m[2])
		}
		if m := grantRE.FindStringSubmatch(line); m != nil {
			db := strings.ReplaceAll(m[1], `\_`, "_")
			if !slices.Contains(dbUsers[db], m[2]) {
				dbUsers[db] = append(dbUsers[db], m[2])
			}
		}
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for d := range dumps {
			if !yield(d) {
				return
			}
		}
	})
	for _, name := range names {
		d := agentproto.CPanelDatabase{Name: name, Bytes: dumps[name]}
		// The database's own user, rather than the account's main user (which can use all).
		for _, u := range dbUsers[name] {
			if d.User == "" || (d.User == plan.User && u != plan.User) {
				d.User = u
			}
		}
		if d.User != "" {
			d.PasswordHash = hashes[d.User]
		}
		plan.Databases = append(plan.Databases, d)
	}
	for k, v := range small {
		if !strings.HasPrefix(k, "cron/") {
			continue
		}
		for _, l := range strings.Split(v, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if cronEnvLineRE.MatchString(l) {
				// An empty MAILTO or the usual shell change nothing worth telling.
				if !slices.Contains([]string{`MAILTO=""`, "MAILTO=", `SHELL="/bin/bash"`, "SHELL=/bin/bash", `SHELL="/bin/sh"`, "SHELL=/bin/sh"}, l) {
					plan.CronSettings = append(plan.CronSettings, l)
				}
				continue
			}
			plan.Cron = append(plan.Cron, l)
		}
	}
	if plan.Domains == nil {
		plan.Domains = []agentproto.CPanelDomain{}
	}
	if plan.Databases == nil {
		plan.Databases = []agentproto.CPanelDatabase{}
	}
	if plan.Cron == nil {
		plan.Cron = []string{}
	}
	return nil
}

// ---- Apply ----

// rewriteMaxSize: larger files are left as they are (logs, dumps, media).
const rewriteMaxSize = 1 << 20

// rewritable says whether a file is configuration or code that may name its home folder.
func rewritable(rel string) bool {
	base := path.Base(rel)
	switch base {
	case ".htaccess", ".user.ini", "php.ini", ".env", "wp-config.php", "configuration.php", "config.php":
		return true
	}
	if strings.HasPrefix(base, ".env.") {
		return true
	}
	switch path.Ext(base) {
	case ".php", ".inc", ".ini", ".env", ".json", ".yml", ".yaml", ".conf", ".cfg", ".sh", ".pl", ".py", ".cgi", ".xml", ".txt":
		return true
	}
	return false
}

// homeRewriter changes /home/<old>/… to where that place is now: a website's document root
// becomes its public_html, anything else the main website's home folder.
type homeRewriter struct {
	re    *regexp.Regexp
	roots map[string]string // old document root → new public_html
	home  string            // new home for the rest ("" leaves the rest as it is)
}

func newHomeRewriter(oldUser string, sites []agentproto.ImportSite, mainUser string) *homeRewriter {
	if !agentproto.ValidCPanelUser(oldUser) {
		return nil
	}
	h := &homeRewriter{roots: map[string]string{}}
	if mainUser != "" {
		h.home = "/home/" + mainUser
	}
	var alts []string
	for _, s := range sites { // longest first already
		h.roots[s.DocRoot] = "/home/" + s.User + "/public_html"
		alts = append(alts, regexp.QuoteMeta(s.DocRoot))
	}
	root := ""
	if len(alts) > 0 {
		root = `(/(?:` + strings.Join(alts, "|") + `))?`
	}
	// The old home, a document root in it if any, then something that ends the name.
	h.re = regexp.MustCompile(regexp.QuoteMeta("/home/"+oldUser) + root + `([^A-Za-z0-9_.\-]|$)`)
	return h
}

func (h *homeRewriter) rewrite(b []byte) ([]byte, bool) {
	if bytes.IndexByte(b, 0) >= 0 {
		return b, false // not text
	}
	changed := false
	out := h.re.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := h.re.FindSubmatch(m)
		var root, end []byte
		if len(h.roots) > 0 {
			root, end = sub[1], sub[2]
		} else {
			end = sub[1]
		}
		to := h.home
		if len(root) > 0 {
			to = h.roots[string(root[1:])]
		}
		if to == "" {
			return m
		}
		changed = true
		return append([]byte(to), end...)
	})
	return out, changed
}

func importApply(ctx context.Context, s *system, p agentproto.ImportApplyParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	for _, site := range p.Sites {
		if !userRE.MatchString(site.User) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid user")
		}
	}
	f, tr, err := openArchive(s, p.File)
	if err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	defer f.Close()
	// Longest document root first, so an addon's folder inside public_html goes to the addon.
	sites := slices.Clone(p.Sites)
	sort.Slice(sites, func(i, j int) bool { return len(sites[i].DocRoot) > len(sites[j].DocRoot) })
	var res agentproto.ImportApplyResult
	touched := map[string]bool{} // homes to give to their accounts afterwards
	skipped := map[string]int{}
	paths := newHomeRewriter(p.OldUser, sites, p.MainUser)
	// Every write goes through the home folder's os.Root: links in the archive (each checked
	// alone, but chained) cannot lead a root-owned write out of the website's home.
	roots := map[string]*os.Root{}
	defer func() {
		for _, r := range roots {
			r.Close()
		}
	}()
	rootOf := func(user string) (*os.Root, error) {
		if r, ok := roots[user]; ok {
			return r, nil
		}
		r, err := os.OpenRoot(s.path("/home/" + user))
		if err != nil {
			return nil, err
		}
		roots[user] = r
		return r, nil
	}

	place := func(rel string, h *tar.Header, r io.Reader) error {
		user, dest := "", ""
		for _, site := range sites {
			if rel == site.DocRoot || strings.HasPrefix(rel, site.DocRoot+"/") {
				user = site.User
				dest = path.Join("/home", site.User, "public_html", strings.TrimPrefix(strings.TrimPrefix(rel, site.DocRoot), "/"))
				break
			}
		}
		if user == "" {
			top := strings.SplitN(rel, "/", 2)[0]
			if p.MainUser == "" || slices.Contains(homeSkip, top) || top == "public_html" || strings.HasPrefix(top, ".") && top != ".well-known" {
				skipped[top]++
				return nil
			}
			user, dest = p.MainUser, path.Join("/home", p.MainUser, rel)
		}
		home := "/home/" + user
		name, ok := strings.CutPrefix(dest, home+"/")
		if !ok {
			return nil
		}
		root, err := rootOf(user)
		if err != nil {
			return err
		}
		// A path that leads out of the home (through links) is left out, not an error.
		escapes := func(err error) bool { return err != nil && strings.Contains(err.Error(), "escapes from parent") }
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o750); escapes(err) {
				skipped["links_outside"]++
				return nil
			} else if err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(path.Dir(name), 0o750); escapes(err) {
				skipped["links_outside"]++
				return nil
			} else if err != nil {
				return err
			}
			out, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(h.Mode)&0o777)
			if escapes(err) {
				skipped["links_outside"]++
				return nil
			} else if err != nil {
				return err
			}
			if paths != nil && h.Size <= rewriteMaxSize && rewritable(rel) {
				b, err := io.ReadAll(r)
				if err != nil {
					out.Close()
					return err
				}
				if nb, changed := paths.rewrite(b); changed {
					b = nb
					res.PathsChangedCount++
					if len(res.PathsChanged) < 50 {
						res.PathsChanged = append(res.PathsChanged, strings.TrimPrefix(dest, "/home/"))
					}
				}
				r = bytes.NewReader(b)
			}
			n, err := io.Copy(out, r)
			out.Close()
			if err != nil {
				return err
			}
			res.Files++
			res.Bytes += n
		case tar.TypeSymlink:
			// Only links that stay inside the website.
			target := path.Clean(path.Join(path.Dir(dest), h.Linkname))
			if path.IsAbs(h.Linkname) || (target != home && !strings.HasPrefix(target, home+"/")) {
				skipped["links_outside"]++
				return nil
			}
			if err := root.MkdirAll(path.Dir(name), 0o750); escapes(err) {
				skipped["links_outside"]++
				return nil
			} else if err != nil {
				return err
			}
			root.Remove(name)
			if err := root.Symlink(h.Linkname, name); escapes(err) {
				skipped["links_outside"]++
				return nil
			} else if err != nil {
				return err
			}
		default:
			skipped["special_files"]++
			return nil
		}
		touched[user] = true
		return nil
	}

	// A site's fresh public_html (the panel's placeholder page) gives way to the copy.
	for _, site := range sites {
		os.RemoveAll(s.path(path.Join("/home", site.User, "public_html", "index.html")))
	}
	wanted := map[string]agentproto.TransferDatabase{}
	for _, d := range p.Databases {
		wanted[d.Name] = d
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "read the archive: %v", err)
		}
		rel := inArchive(h.Name)
		handled, err := homeEntries(tr, h, rel, place)
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		if handled {
			continue
		}
		if name, ok := strings.CutPrefix(rel, "mysql/"); ok && strings.HasSuffix(name, ".sql") && h.Typeflag == tar.TypeReg {
			d, ok := wanted[strings.TrimSuffix(name, ".sql")]
			if !ok {
				continue
			}
			if err := s.loadDumpFrom(ctx, tr, d); err != nil {
				return nil, opError(agentproto.ErrFailed, "%v", err)
			}
			res.Databases = append(res.Databases, d.Name)
		}
	}
	for user := range touched {
		home := s.path("/home/" + user)
		entries, _ := os.ReadDir(home)
		for _, e := range entries {
			// The home folder itself stays root's (SFTP); everything in it is the account's.
			if _, err := s.run(ctx, "chown", "-R", user+":"+user, filepath.Join(home, e.Name())); err != nil {
				return nil, opError(agentproto.ErrFailed, "%v", err)
			}
		}
		if _, err := s.run(ctx, "setfacl", "-R", "-m", "u:caddy:rX,d:u:caddy:rX", filepath.Join(home, "public_html")); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
	}
	for what, n := range skipped {
		res.Skipped = append(res.Skipped, agentproto.ImportSkip{Name: what, Count: n})
	}
	sort.Slice(res.Skipped, func(i, j int) bool { return res.Skipped[i].Name < res.Skipped[j].Name })
	return res, nil
}

// loadDumpFrom loads a dump read from r into an existing database, as loadDump does.
func (s *system) loadDumpFrom(ctx context.Context, r io.Reader, d agentproto.TransferDatabase) error {
	var stderr strings.Builder
	c := command{stderr: &stderr}
	if d.Engine == "postgresql" {
		c.name = "runuser"
		c.args = []string{"-u", "postgres", "--", "psql", "-q", "-v", "ON_ERROR_STOP=1", "-d", d.Name}
		c.stdin = io.MultiReader(strings.NewReader("SET ROLE "+d.User+";\n"), r)
	} else {
		c.name, c.args, c.stdin = "mysql", []string{"--default-character-set=utf8mb4", d.Name}, r
	}
	if _, err := s.exec(ctx, c); err != nil {
		return fmt.Errorf("load the dump of %s: %w: %s", d.Name, err, lastLines([]byte(stderr.String()), 5))
	}
	return nil
}
