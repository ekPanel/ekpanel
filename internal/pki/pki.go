// Package pki is the panel's private certificate authority for agent connections.
//
// The panel creates a CA on first start. Agents pin that CA during enrolment (the
// enrolment command carries its SHA-256 fingerprint) and from then on talk to the panel
// over mutual TLS: the panel proves itself with a certificate from the CA, and each agent
// with its own client certificate whose common name is its server ID.
//
// Because the CA is private and pinned, agents verify the panel's certificate chain but
// not its host name. Panels are often reached by IP address or through NAT, and the pin is
// a stronger check than a name would be.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caValidity     = 20 * 365 * 24 * time.Hour
	serverValidity = 365 * 24 * time.Hour
	// ClientValidity is how long an agent certificate lasts. Agents renew well before.
	ClientValidity = 90 * 24 * time.Hour
	// RenewBefore is how close to expiry an agent asks for a new certificate.
	RenewBefore = 30 * 24 * time.Hour
)

type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	key     crypto.Signer
}

// LoadOrCreateCA reads ca.crt and ca.key from dir, creating them on first use.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if certErr == nil && keyErr == nil {
		cert, err := ParseCertPEM(certPEM)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", certPath, err)
		}
		key, err := ParseKeyPEM(keyPEM)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", keyPath, err)
		}
		return &CA{Cert: cert, CertPEM: certPEM, key: key}, nil
	}
	if !errors.Is(certErr, os.ErrNotExist) || !errors.Is(keyErr, os.ErrNotExist) {
		return nil, fmt.Errorf("agent CA is incomplete in %s (need both ca.crt and ca.key)", dir)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "ekPanel agent CA " + hex.EncodeToString(randomBytes(4))},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(caValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, _ := x509.ParseCertificate(der)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err = EncodeKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// Write the key first: a crash between the two writes leaves an incomplete CA that is
	// reported, never a certificate without its key silently replaced.
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	return &CA{Cert: cert, CertPEM: certPEM, key: key}, nil
}

// Pin is the CA's fingerprint as agents receive it: "sha256:<hex of the DER certificate>".
func (ca *CA) Pin() string { return Fingerprint(ca.Cert.Raw) }

// Fingerprint formats the SHA-256 of a DER certificate the way pins and stored agent
// certificate hashes are written.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ServerTLSConfig is for the panel's agent listener. A fresh server certificate is issued
// at every start. Client certificates are verified when presented; the enrolment endpoint
// is reached without one, and every other endpoint requires one.
func (ca *CA) ServerTLSConfig() (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "ekpanel"},
		DNSNames:     []string{"ekpanel", "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(serverValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.key)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Send the CA too, so an enrolling agent can match it against its pin.
		Certificates: []tls.Certificate{{Certificate: [][]byte{der, ca.Cert.Raw}, PrivateKey: key}},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}, nil
}

// SignAgentCSR issues a client certificate for serverID from a PEM certificate request.
// Only the request's public key is used; names and extensions come from the panel.
func (ca *CA) SignAgentCSR(csrPEM []byte, serverID string) (certPEM []byte, fingerprint string, err error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, "", errors.New("not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("certificate request signature: %w", err)
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, "", errors.New("agent keys must be ECDSA")
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: serverID},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(ClientValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), Fingerprint(der), nil
}

// ---- Agent side ----

// NewAgentKey creates an agent's private key and a certificate request for it.
// The key never leaves the agent's server.
func NewAgentKey() (keyPEM, csrPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "ekpanel-agent"}}, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = EncodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return keyPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), nil
}

// PinnedTLSConfig trusts only a panel whose chain contains a CA with fingerprint pin.
// It is used once, for enrolment. found receives that CA's PEM.
func PinnedTLSConfig(pin string, found *[]byte) *tls.Config {
	pin = strings.ToLower(strings.TrimSpace(pin))
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by the pin check below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			for _, der := range raw {
				if Fingerprint(der) != pin {
					continue
				}
				ca, err := x509.ParseCertificate(der)
				if err != nil {
					return err
				}
				if err := verifyLeaf(raw[0], ca); err != nil {
					return err
				}
				*found = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
				return nil
			}
			return errors.New("the panel's certificate authority does not match the --ca-pin from the panel; check the command, or someone may be intercepting the connection")
		},
	}
}

// AgentTLSConfig is the agent's everyday config: its own certificate, and the pinned CA.
func AgentTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	ca, err := ParseCertPEM(caPEM)
	if err != nil {
		return nil, fmt.Errorf("panel CA: %w", err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("agent certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{pair},
		InsecureSkipVerify: true, // chain is checked against the pinned CA below; host names are not used
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("panel sent no certificate")
			}
			return verifyLeaf(raw[0], ca)
		},
	}, nil
}

func verifyLeaf(leafDER []byte, ca *x509.Certificate) error {
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

// ---- Encoding helpers ----

func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func ParseKeyPEM(b []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("not a PEM key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported key type")
	}
	return signer, nil
}

func EncodeKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func randomSerial() *big.Int { return new(big.Int).SetBytes(randomBytes(16)) }
