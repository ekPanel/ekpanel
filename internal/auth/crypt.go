package auth

import (
	"github.com/GehirnInc/crypt"
	_ "github.com/GehirnInc/crypt/sha512_crypt" // registers $6$
)

// linuxCryptRounds makes each guess against a stolen hash cost 100,000 SHA-512 rounds.
const linuxCryptRounds = "rounds=100000"

// LinuxPasswordHash returns a SHA-512 crypt hash ("$6$…") that Linux accepts in
// /etc/shadow, for SFTP sign-in. The password itself is never stored.
func LinuxPasswordHash(password string) (string, error) {
	salt := NewDatabasePassword()[:16]
	return crypt.SHA512.New().Generate([]byte(password), []byte("$6$"+linuxCryptRounds+"$"+salt))
}

// CheckLinuxPassword verifies a password against a crypt hash (used in tests).
func CheckLinuxPassword(hash, password string) bool {
	return crypt.SHA512.New().Verify(hash, []byte(password)) == nil
}

// MailPasswordHash is a SHA-512 crypt hash for a mailbox, with the standard 5,000 rounds:
// the mail server checks it on every sign-in, and webmail and mail apps sign in often.
func MailPasswordHash(password string) (string, error) {
	salt := NewDatabasePassword()[:16]
	return crypt.SHA512.New().Generate([]byte(password), []byte("$6$"+salt))
}
