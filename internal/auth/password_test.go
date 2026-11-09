package auth

import (
	"bytes"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := VerifyPassword("correct horse battery", hash); err != nil || !ok {
		t.Fatalf("right password rejected: ok=%v err=%v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong horse battery", hash); ok {
		t.Fatal("wrong password accepted")
	}
	other, _ := HashPassword("correct horse battery")
	if other == hash {
		t.Fatal("two hashes of the same password must use different salts")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$aa$bb", "$argon2id$v=19$m=x$aa$bb"} {
		if ok, err := VerifyPassword("x", h); ok || err == nil {
			t.Errorf("hash %q: ok=%v err=%v, want rejection", h, ok, err)
		}
	}
}

func TestTokens(t *testing.T) {
	a, b := NewToken(), NewToken()
	if a == b || len(a) < 40 {
		t.Fatalf("tokens not random enough: %q %q", a, b)
	}
	if !bytes.Equal(HashToken(a), HashToken(a)) || bytes.Equal(HashToken(a), HashToken(b)) {
		t.Fatal("HashToken must be deterministic and distinct per token")
	}
}

func TestTOTP(t *testing.T) {
	secret, url, err := NewTOTP("ekPanel", "admin@example.com")
	if err != nil || secret == "" || url == "" {
		t.Fatalf("NewTOTP: %v", err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !CheckTOTP(secret, code) {
		t.Fatal("current code rejected")
	}
	if CheckTOTP(secret, "000000") && code != "000000" {
		t.Fatal("wrong code accepted")
	}
}
