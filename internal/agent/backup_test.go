package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

const repoID = "99999999-8888-7777-6666-555555555555"

type fakeRestic struct {
	*fakeSystem
	mu       sync.Mutex // dumps run in their own goroutine, next to restic
	execs    []command
	stdinGot map[string]string // stdin-filename -> data restic received
	dumpFail bool
	forgot   []string
}

func newFakeRestic(t *testing.T) *fakeRestic {
	f := &fakeRestic{fakeSystem: newFakeSystem(t), stdinGot: map[string]string{}}
	f.installed["restic"] = true
	f.users["shop_ab12"] = []string{"shop_ab12", sitesGroup}
	f.exec = func(_ context.Context, c command) ([]byte, error) {
		f.mu.Lock()
		f.execs = append(f.execs, c)
		f.mu.Unlock()
		switch c.name {
		case "mariadb-dump", "runuser":
			if f.dumpFail {
				io.WriteString(c.stdout, "CREATE TABLE half")
				return nil, errors.New("mariadb-dump: Got error 2013: Lost connection")
			}
			io.WriteString(c.stdout, "CREATE TABLE t (x int);\n")
			return nil, nil
		case "restic":
			switch c.args[0] {
			case "backup":
				if c.stdin != nil {
					b, _ := io.ReadAll(c.stdin)
					f.stdinGot[argAfter(c.args, "--stdin-filename")] = string(b)
					return []byte(`{"message_type":"summary","snapshot_id":"abcdef0123456789"}`), nil
				}
				return []byte(`{"message_type":"status"}` + "\n" + `{"message_type":"summary","snapshot_id":"1234567890abcdef"}`), nil
			case "forget":
				f.forgot = append(f.forgot, strings.Join(c.args[1:], " "))
			case "snapshots":
				fmt.Fprint(c.stdout, `[{"id":"aaaa1111","time":"2026-11-01T02:00:00Z","tags":["ekpanel","kind:site","id:x","name:shop.example.com"]},
					{"id":"bbbb2222","time":"2026-11-02T02:00:00Z","tags":["ekpanel","kind:database","id:y","name:shop"]}]`)
			}
		}
		return nil, nil
	}
	return f
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func envOf(c command, key string) string {
	for _, e := range c.env {
		if v, ok := strings.CutPrefix(e, key+"="); ok {
			return v
		}
	}
	return ""
}

func TestBackupSetupLocalAndS3(t *testing.T) {
	f := newFakeRestic(t)
	res, opErr := backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "local"})
	if opErr != nil {
		t.Fatal(opErr)
	}
	if len(res.RecoveryKey) != 30 || res.Location != backupLocalDir+"/"+repoID {
		t.Fatalf("result %+v", res)
	}
	cfgPath, passPath := repoFiles(f.system, repoID)
	for _, p := range []string{cfgPath, passPath} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s must exist with mode 600", p)
		}
	}
	init := f.execs[len(f.execs)-1]
	if init.args[0] != "init" || envOf(init, "RESTIC_PASSWORD_FILE") != passPath {
		t.Fatalf("init call %+v", init)
	}

	s3 := &agentproto.BackupS3{Endpoint: "s3.eu-central-003.backblazeb2.com", Bucket: "my-backups", Prefix: "web-1", AccessKey: "KEYID", SecretKey: "s3cr3t"}
	if _, opErr := backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "s3", S3: s3}); opErr != nil {
		t.Fatal(opErr)
	}
	init = f.execs[len(f.execs)-1]
	if envOf(init, "RESTIC_REPOSITORY") != "s3:https://s3.eu-central-003.backblazeb2.com/my-backups/web-1" || envOf(init, "AWS_SECRET_ACCESS_KEY") != "s3cr3t" {
		t.Fatalf("s3 env %v", init.env)
	}
	if strings.Contains(strings.Join(init.args, " "), "s3cr3t") {
		t.Fatal("the secret must not be on the command line")
	}
	for _, bad := range []agentproto.BackupS3{
		{Endpoint: "evil.com/x?y", Bucket: "b-ok", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "s3.example.com", Bucket: "../../etc", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "s3.example.com", Bucket: "bucket", Prefix: "../up", AccessKey: "a", SecretKey: "b"},
		{Endpoint: "s3.example.com", Bucket: "bucket", AccessKey: "a\nAWS_X=1", SecretKey: "b"},
	} {
		b := bad
		if _, opErr := backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "s3", S3: &b}); opErr == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func runParams() agentproto.BackupRunParams {
	return agentproto.BackupRunParams{
		RepoID:    repoID,
		Sites:     []agentproto.BackupSite{{ID: siteID, Name: "shop.example.com", User: "shop_ab12"}},
		Databases: []agentproto.BackupDatabase{{ID: dbID, Name: "shop", Engine: "mariadb"}},
		KeepDaily: 7, KeepWeekly: 4,
	}
}

func TestBackupRun(t *testing.T) {
	f := newFakeRestic(t)
	backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "local"})
	res, opErr := backupRun(context.Background(), f.system, runParams())
	if opErr != nil {
		t.Fatal(opErr)
	}
	if len(res.Items) != 2 || !res.Items[0].OK || res.Items[0].SnapshotID != "1234567890abcdef" || !res.Items[1].OK {
		t.Fatalf("items %+v", res.Items)
	}
	var siteBackup command
	for _, c := range f.execs {
		if c.name == "restic" && c.args[0] == "backup" && c.stdin == nil {
			siteBackup = c
		}
	}
	joined := strings.Join(siteBackup.args, " ")
	for _, want := range []string{"--exclude /home/shop_ab12/tmp", "--exclude /home/shop_ab12/.trash", "kind:site,id:" + siteID} {
		if !strings.Contains(joined, want) {
			t.Errorf("site backup lacks %q: %s", want, joined)
		}
	}
	if f.stdinGot["shop.sql"] != "CREATE TABLE t (x int);\n" {
		t.Fatalf("dump not streamed into restic: %q", f.stdinGot)
	}
	if len(f.forgot) != 1 || !strings.Contains(f.forgot[0], "--group-by tags --prune") || !strings.Contains(f.forgot[0], "--keep-daily 7") {
		t.Fatalf("retention: %v", f.forgot)
	}
}

func TestPartialDumpIsNotKept(t *testing.T) {
	f := newFakeRestic(t)
	f.dumpFail = true
	backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "local"})
	p := runParams()
	p.KeepDaily, p.KeepWeekly = 0, 0
	res, _ := backupRun(context.Background(), f.system, p)
	db := res.Items[1]
	if db.OK || !strings.Contains(db.Error, "dump failed") {
		t.Fatalf("partial dump reported as %+v", db)
	}
	if len(f.forgot) != 1 || f.forgot[0] != "abcdef0123456789" {
		t.Fatalf("the snapshot of a partial dump must be forgotten: %v", f.forgot)
	}
}

func TestSnapshotsAndRestoreValidation(t *testing.T) {
	f := newFakeRestic(t)
	backupSetup(context.Background(), f.system, agentproto.BackupSetupParams{RepoID: repoID, Kind: "local"})
	res, opErr := backupSnapshots(context.Background(), f.system, agentproto.BackupSnapshotsParams{RepoID: repoID})
	if opErr != nil {
		t.Fatal(opErr)
	}
	if len(res.Snapshots) != 2 || res.Snapshots[0].Kind != "database" || res.Snapshots[1].Name != "shop.example.com" {
		t.Fatalf("snapshots (newest first) = %+v", res.Snapshots)
	}
	site := &agentproto.BackupSite{ID: siteID, Name: "shop.example.com", User: "shop_ab12"}
	for _, p := range []agentproto.BackupRestoreParams{
		{RepoID: repoID, SnapshotID: "aaaa1111; rm -rf /", Site: site, Mode: "folder"},
		{RepoID: repoID, SnapshotID: "aaaa1111", Site: &agentproto.BackupSite{ID: siteID, Name: "x.com", User: "root"}, Mode: "folder"},
		{RepoID: repoID, SnapshotID: "aaaa1111", Site: site, Mode: "everything"},
		{RepoID: repoID, SnapshotID: "bbbb2222", Database: &agentproto.BackupDatabase{ID: dbID, Name: "mysql", Engine: "mariadb"}},
	} {
		if _, opErr := backupRestore(context.Background(), f.system, p); opErr == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}
