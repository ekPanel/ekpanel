package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// NewDatabasePassword returns a random 24-character password (about 140 bits). It uses
// only letters and digits, without look-alikes, so it can be pasted into any config file.
func NewDatabasePassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = passwordAlphabet[int(b[i])%len(passwordAlphabet)]
	}
	return string(b)
}

// MariaDBNativeHash is the mysql_native_password hash MariaDB accepts in
// `IDENTIFIED BY PASSWORD '...'`: "*" + uppercase hex of SHA1(SHA1(password)).
func MariaDBNativeHash(password string) string {
	first := sha1.Sum([]byte(password))
	second := sha1.Sum(first[:])
	return "*" + strings.ToUpper(hex.EncodeToString(second[:]))
}

const scramIterations = 4096

// PostgresSCRAMVerifier is the SCRAM-SHA-256 verifier PostgreSQL stores for a password
// (RFC 5802/7677). PostgreSQL accepts it in `PASSWORD '...'` without seeing the password.
func PostgresSCRAMVerifier(password string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	return scramVerifier(password, salt, scramIterations)
}

func scramVerifier(password string, salt []byte, iterations int) string {
	salted := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
	clientKey := hmacSHA256(salted, "Client Key")
	storedKey := sha256.Sum256(clientKey)
	serverKey := hmacSHA256(salted, "Server Key")
	b64 := base64.StdEncoding
	return fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations,
		b64.EncodeToString(salt), b64.EncodeToString(storedKey[:]), b64.EncodeToString(serverKey))
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
