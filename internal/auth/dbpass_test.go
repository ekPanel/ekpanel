package auth

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestDatabasePassword(t *testing.T) {
	a, b := NewDatabasePassword(), NewDatabasePassword()
	if len(a) != 24 || a == b || !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(a) {
		t.Fatalf("bad passwords %q %q", a, b)
	}
	if strings.ContainsAny(a+b, "0O1lI") {
		t.Fatal("passwords must not contain look-alike characters")
	}
}

func TestMariaDBNativeHash(t *testing.T) {
	// Known value: SELECT PASSWORD('password') in MariaDB/MySQL.
	if got := MariaDBNativeHash("password"); got != "*2470C0C06DEE42FD1618BB99005ADCA2EC9D1E19" {
		t.Fatalf("got %s", got)
	}
}

func TestPostgresSCRAMVerifier(t *testing.T) {
	// Fixed salt reproduces a verifier made by PostgreSQL itself for the password "pencil"
	// with salt "W22ZaJ0SNY7soEsUEjb6gQ==" (RFC 7677 test vector inputs).
	salt, _ := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	got := scramVerifier("pencil", salt, 4096)
	want := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$WG5d8oPm3OtcPnkdi4Uo7BkeZkBFzpcXkuLmtbsT4qY=:wfPLwcE6nTWhTAmQ7tl2KeoiWGPlZqQxSrmfPwDl2dU="
	if got != want {
		t.Fatalf("verifier mismatch\n got %s\nwant %s", got, want)
	}
	v1, v2 := PostgresSCRAMVerifier("x"), PostgresSCRAMVerifier("x")
	if v1 == v2 || !strings.HasPrefix(v1, "SCRAM-SHA-256$4096:") {
		t.Fatal("each verifier needs its own salt")
	}
}
