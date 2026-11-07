package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"ekpanel/internal/agentproto"
)

// registryPath records every database and account the agent created. The agent refuses to
// change or drop any that exist but are not listed, so it can never take over or delete a
// database someone made by hand.
const registryPath = "/var/lib/ekpanel-agent/databases.json"

var (
	dbNameRE      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	mariadbHashRE = regexp.MustCompile(`^\*[0-9A-F]{40}$`)
	scramRE       = regexp.MustCompile(`^SCRAM-SHA-256\$[0-9]{4,7}:[A-Za-z0-9+/=]{4,64}\$[A-Za-z0-9+/=]{44}:[A-Za-z0-9+/=]{44}$`)
	reservedDB    = []string{"root", "admin", "mysql", "mariadb", "information_schema", "performance_schema", "sys",
		"postgres", "template0", "template1", "ekpanel", "public", "pg_signal_backend", "debian_sys_maint", "phpmyadmin"}
)

func validateDatabase(d agentproto.DatabaseSpec) error {
	if !uuidRE.MatchString(d.ID) {
		return errors.New("invalid database id")
	}
	for _, n := range []string{d.Name, d.User} {
		if !dbNameRE.MatchString(n) || slices.Contains(reservedDB, n) || strings.HasPrefix(n, "pg_") {
			return fmt.Errorf("invalid name %q", n)
		}
	}
	if d.Deleted {
		return nil
	}
	switch d.Engine {
	case "mariadb":
		if !mariadbHashRE.MatchString(d.PasswordHash) {
			return errors.New("invalid MariaDB password hash")
		}
	case "postgresql":
		if !scramRE.MatchString(d.PasswordHash) {
			return errors.New("invalid PostgreSQL password verifier")
		}
	default:
		return fmt.Errorf("unknown engine %q", d.Engine)
	}
	return nil
}

// ---- Registry ----

type registry struct {
	sys     *system
	entries map[string]bool // "<engine>:db:<name>" and "<engine>:user:<name>"
}

func loadRegistry(sys *system) (*registry, error) {
	r := &registry{sys: sys, entries: map[string]bool{}}
	b, err := os.ReadFile(sys.path(registryPath))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("read %s: %w", registryPath, err)
	}
	for _, k := range list {
		r.entries[k] = true
	}
	return r, nil
}

func (r *registry) has(key string) bool { return r.entries[key] }

func (r *registry) set(key string, on bool) error {
	if r.entries[key] == on {
		return nil
	}
	if on {
		r.entries[key] = true
	} else {
		delete(r.entries, key)
	}
	list := make([]string, 0, len(r.entries))
	for k := range r.entries {
		list = append(list, k)
	}
	slices.Sort(list)
	b, _ := json.MarshalIndent(list, "", "  ")
	p := r.sys.path(registryPath)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// ---- Engines ----

// engine runs SQL for one database server. SQL goes through stdin, never the command line.
type engine struct {
	name    string // "mariadb" or "postgresql"
	pkg     string // Debian package
	service string // systemd unit
	sys     *system
}

func engineFor(sys *system, name string) *engine {
	if name == "mariadb" {
		return &engine{name: "mariadb", pkg: "mariadb-server", service: "mariadb", sys: sys}
	}
	return &engine{name: "postgresql", pkg: "postgresql", service: "postgresql", sys: sys}
}

func (e *engine) sql(ctx context.Context, sql string) (string, error) {
	var out []byte
	var err error
	if e.name == "mariadb" {
		// root signs in through the unix socket; no password exists for it.
		out, err = e.sys.runInput(ctx, sql, "mariadb", "--batch", "--skip-column-names")
	} else {
		out, err = e.sys.runInput(ctx, sql, "runuser", "-u", "postgres", "--", "psql", "-X", "-q", "-t", "-A", "-v", "ON_ERROR_STOP=1", "-d", "postgres")
	}
	return strings.TrimSpace(string(out)), err
}

func (e *engine) ensureInstalled(ctx context.Context) error {
	out, _ := e.sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", e.pkg)
	if !strings.Contains(string(out), "install ok installed") {
		install := []string{"-o", "DPkg::Lock::Timeout=300", "install", "-y", "--no-install-recommends", e.pkg}
		if _, err := e.sys.run(ctx, "apt-get", install...); err != nil {
			// The package lists may be stale on a server that never ran apt-get update.
			if _, uerr := e.sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "update"); uerr != nil {
				return uerr
			}
			if _, err := e.sys.run(ctx, "apt-get", install...); err != nil {
				return fmt.Errorf("install %s: %w", e.pkg, err)
			}
		}
	}
	_, err := e.sys.run(ctx, "systemctl", "enable", "--now", e.service)
	return err
}

// exists reports whether a database and an account with the given names are present.
func (e *engine) exists(ctx context.Context, db, user string) (dbExists, userExists bool, err error) {
	var q string
	if e.name == "mariadb" {
		q = fmt.Sprintf("SELECT (SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = '%s'), "+
			"(SELECT COUNT(*) FROM mysql.user WHERE User = '%s');", db, user)
	} else {
		q = fmt.Sprintf("SELECT (SELECT COUNT(*) FROM pg_database WHERE datname = '%s') || ' ' || "+
			"(SELECT COUNT(*) FROM pg_roles WHERE rolname = '%s');", db, user)
	}
	out, err := e.sql(ctx, q)
	if err != nil {
		return false, false, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return false, false, fmt.Errorf("unexpected answer %q", out)
	}
	return f[0] != "0", f[1] != "0", nil
}

func (e *engine) ensure(ctx context.Context, d agentproto.DatabaseSpec, dbExists, userExists bool) error {
	var sql string
	if e.name == "mariadb" {
		// Two hosts, so both the socket (localhost) and TCP (127.0.0.1) work for applications.
		var b strings.Builder
		fmt.Fprintf(&b, "CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n", d.Name)
		for _, host := range []string{"localhost", "127.0.0.1"} {
			fmt.Fprintf(&b, "CREATE USER IF NOT EXISTS '%s'@'%s' IDENTIFIED BY PASSWORD '%s';\n", d.User, host, d.PasswordHash)
			fmt.Fprintf(&b, "ALTER USER '%s'@'%s' IDENTIFIED BY PASSWORD '%s';\n", d.User, host, d.PasswordHash)
			fmt.Fprintf(&b, "GRANT ALL PRIVILEGES ON `%s`.* TO '%s'@'%s';\n", d.Name, d.User, host)
		}
		sql = b.String()
	} else {
		var b strings.Builder
		if userExists {
			fmt.Fprintf(&b, "ALTER ROLE \"%s\" WITH LOGIN PASSWORD '%s';\n", d.User, d.PasswordHash)
		} else {
			fmt.Fprintf(&b, "CREATE ROLE \"%s\" WITH LOGIN PASSWORD '%s';\n", d.User, d.PasswordHash)
		}
		if !dbExists {
			// CREATE DATABASE cannot run inside a transaction block, so it is its own statement.
			fmt.Fprintf(&b, "CREATE DATABASE \"%s\" OWNER \"%s\" ENCODING 'UTF8' TEMPLATE template0;\n", d.Name, d.User)
		}
		fmt.Fprintf(&b, "ALTER DATABASE \"%s\" OWNER TO \"%s\";\n", d.Name, d.User)
		// Other accounts on the server may not even connect to it.
		fmt.Fprintf(&b, "REVOKE ALL ON DATABASE \"%s\" FROM PUBLIC;\n", d.Name)
		sql = b.String()
	}
	_, err := e.sql(ctx, sql)
	return err
}

func (e *engine) drop(ctx context.Context, d agentproto.DatabaseSpec) error {
	var sql string
	if e.name == "mariadb" {
		sql = fmt.Sprintf("DROP DATABASE IF EXISTS `%s`;\nDROP USER IF EXISTS '%s'@'localhost', '%s'@'127.0.0.1';\n", d.Name, d.User, d.User)
	} else {
		sql = fmt.Sprintf("DROP DATABASE IF EXISTS \"%s\" WITH (FORCE);\nDROP ROLE IF EXISTS \"%s\";\n", d.Name, d.User)
	}
	_, err := e.sql(ctx, sql)
	return err
}

func (e *engine) size(ctx context.Context, db string) *int64 {
	var q string
	if e.name == "mariadb" {
		q = fmt.Sprintf("SELECT COALESCE(SUM(data_length + index_length), 0) FROM information_schema.TABLES WHERE table_schema = '%s';", db)
	} else {
		q = fmt.Sprintf("SELECT pg_database_size('%s');", db)
	}
	out, err := e.sql(ctx, q)
	if err != nil {
		return nil
	}
	n, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// ---- Apply ----

func applyDatabases(ctx context.Context, sys *system, specs []agentproto.DatabaseSpec) []agentproto.DatabaseResult {
	results := make([]agentproto.DatabaseResult, 0, len(specs))
	reg, regErr := loadRegistry(sys)
	installed := map[string]error{}
	for _, d := range specs {
		r := agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true}
		err := regErr
		if err == nil {
			err = validateDatabase(d)
		}
		if err == nil {
			err = applyDatabase(ctx, sys, reg, installed, d, &r)
		}
		if err != nil {
			r.OK, r.Error = false, err.Error()
		}
		results = append(results, r)
	}
	return results
}

func applyDatabase(ctx context.Context, sys *system, reg *registry, installed map[string]error, d agentproto.DatabaseSpec, r *agentproto.DatabaseResult) error {
	e := engineFor(sys, d.Engine)
	dbKey, userKey := d.Engine+":db:"+d.Name, d.Engine+":user:"+d.User

	if d.Deleted {
		if !reg.has(dbKey) && !reg.has(userKey) {
			return nil // never created here (or already removed): nothing to do
		}
		if err := e.drop(ctx, d); err != nil {
			return err
		}
		if err := reg.set(dbKey, false); err != nil {
			return err
		}
		return reg.set(userKey, false)
	}

	if err, done := installed[d.Engine]; done {
		if err != nil {
			return err
		}
	} else {
		err := e.ensureInstalled(ctx)
		installed[d.Engine] = err
		if err != nil {
			return err
		}
	}
	dbExists, userExists, err := e.exists(ctx, d.Name, d.User)
	if err != nil {
		return err
	}
	if dbExists && !reg.has(dbKey) {
		return fmt.Errorf("a database named %q already exists but was not created by ekPanel; refusing to use it", d.Name)
	}
	if userExists && !reg.has(userKey) {
		return fmt.Errorf("an account named %q already exists but was not created by ekPanel; refusing to use it", d.User)
	}
	// Record first, so a crash between creating and recording cannot orphan them.
	if err := reg.set(dbKey, true); err != nil {
		return err
	}
	if err := reg.set(userKey, true); err != nil {
		return err
	}
	if err := e.ensure(ctx, d, dbExists, userExists); err != nil {
		return err
	}
	r.SizeBytes = e.size(ctx, d.Name)
	return nil
}
