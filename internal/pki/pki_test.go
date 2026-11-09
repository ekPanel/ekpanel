package pki

import (
	"crypto/tls"
	"io"
	"strings"
	"testing"
)

func TestCAIsCreatedOnceAndReloaded(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.Pin() != b.Pin() || !strings.HasPrefix(a.Pin(), "sha256:") {
		t.Fatalf("pins differ after reload: %s vs %s", a.Pin(), b.Pin())
	}
}

// handshake runs a TLS 1.3 handshake between the panel and an agent config and returns
// the client certificate the panel saw (if any).
func handshake(t *testing.T, server, client *tls.Config) ([]byte, error) {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	seen := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			seen <- nil
			return
		}
		defer c.Close()
		tc := c.(*tls.Conn)
		if err := tc.Handshake(); err != nil {
			seen <- nil
			return
		}
		st := tc.ConnectionState()
		if len(st.VerifiedChains) > 0 {
			seen <- st.PeerCertificates[0].Raw
		} else {
			seen <- nil
		}
		io.Copy(io.Discard, c)
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err != nil {
		return nil, err
	}
	// TLS 1.3 client auth completes on the first read/write.
	conn.Write([]byte("x"))
	conn.Close()
	return <-seen, nil
}

func TestEnrolmentPinAndMutualTLS(t *testing.T) {
	ca, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server, err := ca.ServerTLSConfig()
	if err != nil {
		t.Fatal(err)
	}

	// Enrolment: the right pin succeeds and hands back the CA.
	var caPEM []byte
	if _, err := handshake(t, server, PinnedTLSConfig(ca.Pin(), &caPEM)); err != nil {
		t.Fatalf("pinned handshake: %v", err)
	}
	if string(caPEM) != string(ca.CertPEM) {
		t.Fatal("pinned handshake did not return the CA")
	}

	// A different CA (an attacker in the middle) is refused.
	other, _ := LoadOrCreateCA(t.TempDir())
	var ignored []byte
	if _, err := handshake(t, server, PinnedTLSConfig(other.Pin(), &ignored)); err == nil {
		t.Fatal("handshake with the wrong pin must fail")
	}

	// The panel signs the agent's key; the agent then authenticates with it.
	keyPEM, csrPEM, err := NewAgentKey()
	if err != nil {
		t.Fatal(err)
	}
	certPEM, fp, err := ca.SignAgentCSR(csrPEM, "server-123")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := ParseCertPEM(certPEM)
	if cert.Subject.CommonName != "server-123" || Fingerprint(cert.Raw) != fp {
		t.Fatalf("issued cert: cn=%s", cert.Subject.CommonName)
	}
	agentCfg, err := AgentTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	seen, err := handshake(t, server, agentCfg)
	if err != nil {
		t.Fatalf("mTLS handshake: %v", err)
	}
	if Fingerprint(seen) != fp {
		t.Fatal("panel did not see the agent's verified certificate")
	}

	// An agent certificate from another CA is not verified by this panel.
	k2, c2, _ := NewAgentKey()
	foreign, _, _ := other.SignAgentCSR(c2, "server-123")
	foreignCfg, _ := AgentTLSConfig(caPEM, foreign, k2)
	if seen, _ := handshake(t, server, foreignCfg); seen != nil {
		t.Fatal("a certificate from another CA must not be accepted")
	}
}

func TestSignRejectsGarbage(t *testing.T) {
	ca, _ := LoadOrCreateCA(t.TempDir())
	if _, _, err := ca.SignAgentCSR([]byte("nope"), "x"); err == nil {
		t.Fatal("garbage CSR accepted")
	}
}
