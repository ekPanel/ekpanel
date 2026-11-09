package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

const (
	backupConfigDir = "/etc/ekpanel/backup"
	backupLocalDir  = "/var/backups/ekpanel"
	resticCacheDir  = "/var/cache/ekpanel-restic"
)

var (
	s3EndpointRE = regexp.MustCompile(`^(https?://)?[A-Za-z0-9.-]+(:[0-9]{1,5})?$`)
	s3BucketRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	s3PrefixRE   = regexp.MustCompile(`^[A-Za-z0-9._/-]{0,200}$`)
	s3RegionRE   = regexp.MustCompile(`^[a-z0-9-]{0,40}$`)
	snapshotRE   = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	recoveryRE   = regexp.MustCompile(`^[A-Za-z0-9]{16,128}$`)
)

// repoConfig is what the agent keeps per repository, readable by root only.
type repoConfig struct {
	Repository string   `json:"repository"` // restic repository URL
	Env        []string `json:"env"`        // storage credentials
	Location   string   `json:"location"`   // the same, without secrets, for people
}

func repoFiles(sys *system, id string) (cfg, pass string) {
	return sys.path(backupConfigDir + "/" + id + ".json"), sys.path(backupConfigDir + "/" + id + ".pass")
}

func loadRepo(sys *system, id string) (*repoConfig, error) {
	if !uuidRE.MatchString(id) {
		return nil, errors.New("invalid repository id")
	}
	cfgPath, _ := repoFiles(sys, id)
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("this server has no settings for the backup location (set it up again): %w", err)
	}
	var c repoConfig
	return &c, json.Unmarshal(b, &c)
}

// restic runs restic against a repository with its password file and credentials.
func restic(ctx context.Context, sys *system, id string, cfg *repoConfig, c command) ([]byte, error) {
	_, pass := repoFiles(sys, id)
	c.env = append([]string{"RESTIC_REPOSITORY=" + cfg.Repository, "RESTIC_PASSWORD_FILE=" + pass,
		"RESTIC_CACHE_DIR=" + sys.path(resticCacheDir), "HOME=/root"}, append(cfg.Env, c.env...)...)
	c.name = "restic"
	return sys.exec(ctx, c)
}

func ensureRestic(ctx context.Context, sys *system) error { return ensurePackage(ctx, sys, "restic") }

// ensurePackage installs a Debian package unless it is there, refreshing the package lists
// once if the first attempt fails.
func ensurePackage(ctx context.Context, sys *system, pkgs ...string) error {
	var missing []string
	for _, pkg := range pkgs {
		out, _ := sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
		if !strings.Contains(string(out), "install ok installed") {
			missing = append(missing, pkg)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	install := append([]string{"-o", "DPkg::Lock::Timeout=300", "install", "-y", "--no-install-recommends"}, missing...)
	if _, err := sys.run(ctx, "apt-get", install...); err != nil {
		// A fresh server may have no package lists yet.
		if _, uerr := sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "update"); uerr != nil {
			return uerr
		}
		_, err = sys.run(ctx, "apt-get", install...)
		return err
	}
	return nil
}

func validateS3(s *agentproto.BackupS3) error {
	switch {
	case s == nil:
		return errors.New("S3 settings are missing")
	case !s3EndpointRE.MatchString(s.Endpoint):
		return errors.New("invalid S3 endpoint")
	case !s3BucketRE.MatchString(s.Bucket):
		return errors.New("invalid bucket name")
	case !s3PrefixRE.MatchString(s.Prefix) || strings.Contains(s.Prefix, ".."):
		return errors.New("invalid folder (prefix)")
	case !s3RegionRE.MatchString(s.Region):
		return errors.New("invalid region")
	case s.AccessKey == "" || s.SecretKey == "" || len(s.AccessKey) > 256 || len(s.SecretKey) > 256 ||
		strings.ContainsAny(s.AccessKey+s.SecretKey, "\n\r\x00"):
		return errors.New("invalid access key or secret")
	}
	return nil
}

func backupSetup(ctx context.Context, sys *system, p agentproto.BackupSetupParams) (*agentproto.BackupSetupResult, *agentproto.OpError) {
	if !uuidRE.MatchString(p.RepoID) {
		return nil, opError(agentproto.ErrInvalidParams, "invalid repository id")
	}
	if p.RecoveryKey != "" && !recoveryRE.MatchString(p.RecoveryKey) {
		return nil, opError(agentproto.ErrInvalidParams, "the recovery key is not in the expected format")
	}
	cfg := &repoConfig{}
	switch p.Kind {
	case "local":
		if p.RecoveryKey != "" {
			return nil, opError(agentproto.ErrInvalidParams, "only backups in S3 storage can be connected again")
		}
		dir := backupLocalDir + "/" + p.RepoID
		cfg.Repository, cfg.Location = sys.path(dir), dir
	case "s3":
		if err := validateS3(p.S3); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		endpoint := p.S3.Endpoint
		if !strings.Contains(endpoint, "://") {
			endpoint = "https://" + endpoint
		}
		path := p.S3.Bucket
		if prefix := strings.Trim(p.S3.Prefix, "/"); prefix != "" {
			path += "/" + prefix
		}
		cfg.Repository = "s3:" + endpoint + "/" + path
		cfg.Location = cfg.Repository
		cfg.Env = []string{"AWS_ACCESS_KEY_ID=" + p.S3.AccessKey, "AWS_SECRET_ACCESS_KEY=" + p.S3.SecretKey}
		if p.S3.Region != "" {
			cfg.Env = append(cfg.Env, "AWS_DEFAULT_REGION="+p.S3.Region)
		}
	default:
		return nil, opError(agentproto.ErrInvalidParams, "unknown backup location type")
	}
	if err := ensureRestic(ctx, sys); err != nil {
		return nil, opError(agentproto.ErrFailed, "install restic: %v", err)
	}

	key := p.RecoveryKey
	if key == "" {
		key = randomKey()
	}
	cfgPath, passPath := repoFiles(sys, p.RepoID)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(cfgPath, b, 0o600); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if err := os.WriteFile(passPath, []byte(key+"\n"), 0o600); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	undo := func() { os.Remove(cfgPath); os.Remove(passPath) }

	if p.RecoveryKey != "" {
		// Connect: the key must open the existing repository.
		if _, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"cat", "config"}}); err != nil {
			undo()
			if strings.Contains(err.Error(), "wrong password") {
				return nil, opError(agentproto.ErrWrongKey, "the recovery key does not open these backups")
			}
			return nil, opError(agentproto.ErrFailed, "could not open the existing backups with this recovery key: %v", err)
		}
		return &agentproto.BackupSetupResult{Location: cfg.Location}, nil
	}
	if p.Kind == "local" {
		os.MkdirAll(sys.path(backupLocalDir), 0o700)
	}
	if _, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"init"}}); err != nil {
		undo()
		return nil, opError(agentproto.ErrFailed, "create the backup repository: %v", err)
	}
	return &agentproto.BackupSetupResult{RecoveryKey: key, Location: cfg.Location}, nil
}

// randomKey makes a 30-character recovery key (about 175 bits) without look-alike characters.
func randomKey() string {
	b := make([]byte, 30)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = alphabet[int(c)%len(alphabet)]
	}
	return string(out)
}

// ---- Run ----

type resticSummary struct {
	MessageType string `json:"message_type"`
	SnapshotID  string `json:"snapshot_id"`
}

func snapshotFromJSON(out []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	id := ""
	for sc.Scan() {
		var s resticSummary
		if json.Unmarshal(sc.Bytes(), &s) == nil && s.MessageType == "summary" {
			id = s.SnapshotID
		}
	}
	return id
}

func tagsFor(kind, id, name string) string {
	return strings.Join([]string{"ekpanel", "kind:" + kind, "id:" + id, "name:" + name}, ",")
}

// dumpCommand returns how to dump one database, and the file name it gets in the backup.
func dumpCommand(d agentproto.BackupDatabase) (command, string) {
	if d.Engine == "mariadb" {
		return command{name: "mariadb-dump", args: []string{"--single-transaction", "--routines", "--triggers", "--events", d.Name}}, d.Name + ".sql"
	}
	return command{name: "runuser", args: []string{"-u", "postgres", "--", "pg_dump", "-Fc", d.Name}}, d.Name + ".pgdump"
}

func backupRun(ctx context.Context, sys *system, p agentproto.BackupRunParams) (*agentproto.BackupRunResult, *agentproto.OpError) {
	cfg, err := loadRepo(sys, p.RepoID)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	// A crashed earlier run may have left a stale lock.
	restic(ctx, sys, p.RepoID, cfg, command{args: []string{"unlock"}})
	res := &agentproto.BackupRunResult{}

	for _, s := range p.Sites {
		r := agentproto.BackupItemResult{Kind: "site", ID: s.ID, Name: s.Name}
		if err := validateBackupSite(ctx, sys, s); err != nil {
			r.Error = err.Error()
		} else {
			home := "/home/" + s.User
			out, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"backup", "--json", "--quiet",
				"--tag", tagsFor("site", s.ID, s.Name), "--exclude", home + "/tmp", "--exclude", home + "/.trash", sys.path(home)}})
			if err != nil {
				r.Error = err.Error()
			} else {
				r.OK, r.SnapshotID = true, snapshotFromJSON(out)
			}
		}
		res.Items = append(res.Items, r)
	}

	for _, d := range p.Databases {
		r := agentproto.BackupItemResult{Kind: "database", ID: d.ID, Name: d.Name}
		if err := validateBackupDB(d); err != nil {
			r.Error = err.Error()
			res.Items = append(res.Items, r)
			continue
		}
		dump, file := dumpCommand(d)
		pr, pw := io.Pipe()
		dumpErr := make(chan error, 1)
		go func() {
			dump.stdout = pw
			_, err := sys.exec(ctx, dump)
			pw.CloseWithError(err)
			dumpErr <- err
		}()
		out, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"backup", "--json", "--quiet", "--stdin",
			"--stdin-filename", file, "--tag", tagsFor("database", d.ID, d.Name)}, stdin: pr})
		pr.CloseWithError(io.ErrClosedPipe) // stop the dump if restic failed early
		derr := <-dumpErr
		snap := snapshotFromJSON(out)
		switch {
		case derr != nil:
			// The dump stopped half way: never keep a snapshot of a partial dump.
			if snap != "" {
				restic(ctx, sys, p.RepoID, cfg, command{args: []string{"forget", snap}})
			}
			r.Error = "database dump failed: " + derr.Error()
		case err != nil:
			r.Error = err.Error()
		default:
			r.OK, r.SnapshotID = true, snap
		}
		res.Items = append(res.Items, r)
	}

	if p.KeepDaily+p.KeepWeekly+p.KeepMonthly > 0 {
		args := []string{"forget", "--tag", "ekpanel", "--group-by", "tags", "--prune"}
		for flag, n := range map[string]int{"--keep-daily": p.KeepDaily, "--keep-weekly": p.KeepWeekly, "--keep-monthly": p.KeepMonthly} {
			if n > 0 {
				args = append(args, flag, strconv.Itoa(n))
			}
		}
		if _, err := restic(ctx, sys, p.RepoID, cfg, command{args: args}); err != nil {
			res.PruneError = err.Error()
		}
	}
	return res, nil
}

func validateBackupSite(ctx context.Context, sys *system, s agentproto.BackupSite) error {
	if !uuidRE.MatchString(s.ID) || !userRE.MatchString(s.User) || slices.Contains(reserved, s.User) || !hostnameRE.MatchString(s.Name) {
		return errors.New("invalid site")
	}
	out, err := sys.run(ctx, "id", "-nG", s.User)
	if err != nil || !slices.Contains(strings.Fields(string(out)), sitesGroup) {
		return fmt.Errorf("%q is not an ekPanel site account", s.User)
	}
	return nil
}

func validateBackupDB(d agentproto.BackupDatabase) error {
	if !uuidRE.MatchString(d.ID) || !dbNameRE.MatchString(d.Name) || slices.Contains(reservedDB, d.Name) ||
		(d.Engine != "mariadb" && d.Engine != "postgresql") {
		return errors.New("invalid database")
	}
	if d.Engine == "postgresql" && (!dbNameRE.MatchString(d.Owner) || slices.Contains(reservedDB, d.Owner)) {
		return errors.New("invalid database owner")
	}
	return nil
}

// ---- Snapshots ----

type resticSnapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Tags []string  `json:"tags"`
}

func backupSnapshots(ctx context.Context, sys *system, p agentproto.BackupSnapshotsParams) (*agentproto.BackupSnapshotsResult, *agentproto.OpError) {
	cfg, err := loadRepo(sys, p.RepoID)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	var stdout bytes.Buffer
	if _, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"snapshots", "--json", "--tag", "ekpanel"}, stdout: &stdout}); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	var raw []resticSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, opError(agentproto.ErrFailed, "read snapshot list: %v", err)
	}
	res := &agentproto.BackupSnapshotsResult{Snapshots: []agentproto.BackupSnapshot{}}
	for _, s := range raw {
		snap := agentproto.BackupSnapshot{ID: s.ID, Time: s.Time}
		for _, t := range s.Tags {
			k, v, _ := strings.Cut(t, ":")
			switch k {
			case "kind":
				snap.Kind = v
			case "id":
				snap.ItemID = v
			case "name":
				snap.Name = v
			}
		}
		res.Snapshots = append(res.Snapshots, snap)
	}
	slices.SortFunc(res.Snapshots, func(a, b agentproto.BackupSnapshot) int { return b.Time.Compare(a.Time) })
	return res, nil
}

// ---- Restore ----

func backupRestore(ctx context.Context, sys *system, p agentproto.BackupRestoreParams) (*agentproto.BackupRestoreResult, *agentproto.OpError) {
	cfg, err := loadRepo(sys, p.RepoID)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if !snapshotRE.MatchString(p.SnapshotID) {
		return nil, opError(agentproto.ErrInvalidParams, "invalid snapshot")
	}
	stamp := time.Now().UTC().Format("20060102-150405")

	switch {
	case p.Site != nil:
		s := *p.Site
		if err := validateBackupSite(ctx, sys, s); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		home := "/home/" + s.User
		var target string
		switch p.Mode {
		case "folder":
			target = home + "/restored-" + stamp
		case "replace":
			target = home + "/.restore-" + stamp
		default:
			return nil, opError(agentproto.ErrInvalidParams, "choose how to restore")
		}
		if _, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"restore",
			p.SnapshotID + ":" + sys.path(home+"/public_html"), "--target", sys.path(target)}}); err != nil {
			os.RemoveAll(sys.path(target))
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		// The files may carry the numbers of an older account with the same name.
		if _, err := sys.run(ctx, "chown", "-R", s.User+":"+s.User, target); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		if p.Mode == "folder" {
			os.Chmod(sys.path(target), 0o750)
			return &agentproto.BackupRestoreResult{Path: strings.TrimPrefix(target, home)}, nil
		}
		// Swap: the live files go to the trash (recoverable), the restored files take their place.
		live, old := home+"/public_html", home+"/.trash/"+stamp+"-public_html-before-restore"
		if err := os.MkdirAll(sys.path(home+"/.trash"), 0o700); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		if err := os.Rename(sys.path(live), sys.path(old)); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		if err := os.Rename(sys.path(target), sys.path(live)); err != nil {
			os.Rename(sys.path(old), sys.path(live)) // put the live files back
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		os.Chmod(sys.path(live), 0o750)
		if _, err := sys.run(ctx, "setfacl", "-R", "-m", "u:caddy:rX,d:u:caddy:rX", live); err != nil {
			return nil, opError(agentproto.ErrFailed, "%v", err)
		}
		return &agentproto.BackupRestoreResult{Path: strings.TrimPrefix(old, home)}, nil

	case p.Database != nil:
		d := *p.Database
		if err := validateBackupDB(d); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		_, file := dumpCommand(d)
		var load command
		if d.Engine == "mariadb" {
			load = command{name: "mariadb", args: []string{d.Name}}
		} else {
			// Objects are recreated as the database's own account, so the application keeps access.
			load = command{name: "runuser", args: []string{"-u", "postgres", "--", "pg_restore", "--clean", "--if-exists",
				"--no-owner", "--role=" + d.Owner, "-d", d.Name}}
		}
		pr, pw := io.Pipe()
		dumpErr := make(chan error, 1)
		go func() {
			_, err := restic(ctx, sys, p.RepoID, cfg, command{args: []string{"dump", p.SnapshotID, "/" + file}, stdout: pw})
			pw.CloseWithError(err)
			dumpErr <- err
		}()
		load.stdin = pr
		_, err := sys.exec(ctx, load)
		pr.CloseWithError(io.ErrClosedPipe)
		if derr := <-dumpErr; derr != nil {
			return nil, opError(agentproto.ErrFailed, "read the backup: %v", derr)
		}
		if err != nil {
			return nil, opError(agentproto.ErrFailed, "load the backup into the database: %v", err)
		}
		return &agentproto.BackupRestoreResult{}, nil
	}
	return nil, opError(agentproto.ErrInvalidParams, "nothing to restore")
}

// backupRemove forgets a repository's settings on this server. The backups themselves are
// left where they are: they can be connected again with the recovery key.
func backupRemove(sys *system, p agentproto.BackupRemoveParams) (*agentproto.BackupRestoreResult, *agentproto.OpError) {
	if !uuidRE.MatchString(p.RepoID) {
		return nil, opError(agentproto.ErrInvalidParams, "invalid repository id")
	}
	cfgPath, passPath := repoFiles(sys, p.RepoID)
	os.Remove(cfgPath)
	os.Remove(passPath)
	return &agentproto.BackupRestoreResult{}, nil
}
