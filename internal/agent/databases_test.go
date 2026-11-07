package agent

import (
	"context"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
)

// fakeDB extends fakeSystem with a pretend MariaDB/PostgreSQL that remembers what exists.
type fakeDB struct {
	*fakeSystem
	sql      []string            // every SQL batch sent
	existing map[string]bool     // "db:<name>" / "user:<name>"
	argv     map[string][]string // command line of each SQL call, to check secrets are not in it
}

func newFakeDB(t *testing.T) *fakeDB {
	f := &fakeDB{fakeSystem: newFakeSystem(t), existing: map[string]bool{}, argv: map[string][]string{}}
	f.installed["mariadb-server"] = true
	f.installed["postgresql"] = true
	f.runInput = func(_ context.Context, input, name string, args ...string) ([]byte, error) {
		f.sql = append(f.sql, input)
		f.argv[input] = append([]string{name}, args...)
		switch {
		case strings.HasPrefix(input, "SELECT (SELECT COUNT(*)"):
			db, user := "0", "0"
			for k := range f.existing {
				if strings.HasPrefix(k, "db:") && strings.Contains(input, "'"+strings.TrimPrefix(k, "db:")+"'") {
					db = "1"
				}
				if strings.HasPrefix(k, "user:") && strings.Contains(input, "'"+strings.TrimPrefix(k, "user:")+"'") {
					user = "1"
				}
			}
			return []byte(db + "\t" + user), nil
		case strings.Contains(input, "data_length") || strings.Contains(input, "pg_database_size"):
			return []byte("12345\n"), nil
		}
		return nil, nil
	}
	return f
}

func (f *fakeDB) applyDB(t *testing.T, specs ...agentproto.DatabaseSpec) []agentproto.DatabaseResult {
	t.Helper()
	f.sql = nil
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Databases: specs})
	if opErr != nil {
		t.Fatal(opErr)
	}
	return res.Databases
}

func (f *fakeDB) sent(substr string) bool {
	for _, s := range f.sql {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

const dbID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

func mariaSpec() agentproto.DatabaseSpec {
	return agentproto.DatabaseSpec{ID: dbID, Generation: 1, Engine: "mariadb", Name: "shop", User: "shop",
		PasswordHash: auth.MariaDBNativeHash("secret-password")}
}

func TestCreateMariaDBDatabase(t *testing.T) {
	f := newFakeDB(t)
	res := f.applyDB(t, mariaSpec())
	if !res[0].OK || res[0].SizeBytes == nil || *res[0].SizeBytes != 12345 {
		t.Fatalf("result = %+v", res[0])
	}
	hash := mariaSpec().PasswordHash
	for _, want := range []string{
		"CREATE DATABASE IF NOT EXISTS `shop` CHARACTER SET utf8mb4",
		"CREATE USER IF NOT EXISTS 'shop'@'localhost' IDENTIFIED BY PASSWORD '" + hash + "'",
		"GRANT ALL PRIVILEGES ON `shop`.* TO 'shop'@'127.0.0.1'",
	} {
		if !f.sent(want) {
			t.Errorf("missing SQL %q in %v", want, f.sql)
		}
	}
	for input, argv := range f.argv {
		if strings.Contains(strings.Join(argv, " "), hash) {
			t.Fatalf("password hash on the command line: %v (input %q)", argv, input)
		}
	}
	reg, _ := loadRegistry(f.system)
	if !reg.has("mariadb:db:shop") || !reg.has("mariadb:user:shop") {
		t.Fatal("created database not recorded in the registry")
	}
}

func TestRefusesDatabasesNotCreatedByEkPanel(t *testing.T) {
	f := newFakeDB(t)
	f.existing["db:shop"] = true // made by hand, not in the registry
	res := f.applyDB(t, mariaSpec())
	if res[0].OK || !strings.Contains(res[0].Error, "not created by ekPanel") {
		t.Fatalf("foreign database accepted: %+v", res[0])
	}
	if f.sent("CREATE") || f.sent("GRANT") {
		t.Fatal("must not touch a foreign database")
	}
	del := mariaSpec()
	del.Deleted = true
	f.applyDB(t, del)
	if f.sent("DROP") {
		t.Fatal("must never drop a database ekPanel did not create")
	}
}

func TestRejectsSQLInjection(t *testing.T) {
	f := newFakeDB(t)
	for _, mutate := range []func(*agentproto.DatabaseSpec){
		func(d *agentproto.DatabaseSpec) { d.Name = "x`; DROP DATABASE mysql; --" },
		func(d *agentproto.DatabaseSpec) { d.User = "a' OR '1'='1" },
		func(d *agentproto.DatabaseSpec) { d.PasswordHash = "*ABC'; DROP USER root; --" },
		func(d *agentproto.DatabaseSpec) { d.Name = "mysql" },
		func(d *agentproto.DatabaseSpec) { d.User = "root" },
		func(d *agentproto.DatabaseSpec) { d.Engine = "postgresql" }, // MariaDB hash is not a SCRAM verifier
	} {
		d := mariaSpec()
		mutate(&d)
		if res := f.applyDB(t, d); res[0].OK {
			t.Errorf("accepted %+v", d)
		}
	}
	if len(f.sql) != 0 {
		t.Fatalf("invalid input reached the database: %v", f.sql)
	}
}

func TestPostgresCreateUpdateDelete(t *testing.T) {
	f := newFakeDB(t)
	d := agentproto.DatabaseSpec{ID: dbID, Generation: 1, Engine: "postgresql", Name: "app", User: "app_user",
		PasswordHash: auth.PostgresSCRAMVerifier("pw")}
	if res := f.applyDB(t, d); !res[0].OK {
		t.Fatal(res[0].Error)
	}
	for _, want := range []string{
		`CREATE ROLE "app_user" WITH LOGIN PASSWORD 'SCRAM-SHA-256$4096:`,
		`CREATE DATABASE "app" OWNER "app_user"`,
		`REVOKE ALL ON DATABASE "app" FROM PUBLIC`,
	} {
		if !f.sent(want) {
			t.Errorf("missing SQL %q", want)
		}
	}
	if argv := f.argv[f.sql[1]]; strings.Join(argv[:4], " ") != "runuser -u postgres --" {
		t.Errorf("SQL must run as the postgres user: %v", argv)
	}

	// The second run (now existing) changes the password but does not recreate anything.
	f.existing["db:app"], f.existing["user:app_user"] = true, true
	d.Generation, d.PasswordHash = 2, auth.PostgresSCRAMVerifier("new")
	if res := f.applyDB(t, d); !res[0].OK || f.sent("CREATE") || !f.sent(`ALTER ROLE "app_user" WITH LOGIN PASSWORD`) {
		t.Fatalf("update: %+v %v", res[0], f.sql)
	}

	d.Deleted, d.Generation = true, 3
	if res := f.applyDB(t, d); !res[0].OK || !f.sent(`DROP DATABASE IF EXISTS "app" WITH (FORCE)`) || !f.sent(`DROP ROLE IF EXISTS "app_user"`) {
		t.Fatalf("delete: %+v %v", res[0], f.sql)
	}
	if reg, _ := loadRegistry(f.system); reg.has("postgresql:db:app") {
		t.Fatal("deleted database still in the registry")
	}
}
