package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIError is an error answer from the panel: {"code": "…", "message": "…"}. Answers that
// are not ekPanel errors (a proxy's HTML page, say) get the code http_<status>.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s (%s)", e.Message, e.Code) }

// hint says what to do about an error, or "".
func (e *APIError) hint(login string) string {
	switch {
	case e.Status == http.StatusUnauthorized:
		return "Log in again with a valid token: " + login
	case e.Code == "token_scope":
		return "Create a token with that scope in the panel (Account → API tokens), then run: " + login
	case e.Code == "token_not_allowed":
		return "API tokens cannot do this. Use the panel's web interface."
	case strings.HasPrefix(e.Code, "http_"):
		return "This answer did not come from the ekPanel API. Check the panel URL."
	}
	return ""
}

func apiErrorFrom(status int, body []byte) *APIError {
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		if e.Message == "" {
			e.Message = http.StatusText(status)
		}
		return &APIError{Status: status, Code: e.Code, Message: e.Message}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" || strings.HasPrefix(msg, "<") || len(msg) > 200 {
		msg = http.StatusText(status)
	}
	return &APIError{Status: status, Code: fmt.Sprintf("http_%d", status), Message: msg}
}

// client talks to one panel.
type client struct {
	base  string // https://panel.example.com
	token string
	hc    *http.Client
	ua    string
}

// newClient makes a client. pin is the SHA-256 fingerprint of a certificate the user
// trusts for this panel, or "".
func (a *app) newClient(base, token, pin string) *client {
	hc := &http.Client{Timeout: 5 * time.Minute}
	if a.env.Transport != nil {
		hc.Transport = a.env.Transport
	} else {
		t := http.DefaultTransport.(*http.Transport).Clone()
		if pin != "" {
			t.TLSClientConfig = pinnedTLS(pin)
		}
		hc.Transport = t
	}
	return &client{base: base, token: token, hc: hc, ua: "ekctl/" + a.env.Version}
}

// client is the client for the panel in use: --url, EKCTL_URL or the saved one.
func (a *app) client() (*client, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	raw := a.g.URL
	if raw == "" {
		raw = a.env.Getenv("EKCTL_URL")
	}
	if raw == "" {
		raw = cfg.URL
	}
	if raw == "" {
		return nil, errors.New("Not logged in. Run: ekctl login --url https://panel.example.com")
	}
	base, err := normalizeURL(raw)
	if err != nil {
		return nil, usagef("%v", err)
	}
	token := a.g.Token
	if token == "" {
		token = a.env.Getenv("EKCTL_TOKEN")
	}
	// The saved token belongs to the saved panel. It is never sent anywhere else.
	if token == "" && cfg.URL != "" && base == cfg.URL {
		token = cfg.Token
	}
	if token == "" {
		return nil, fmt.Errorf("Not logged in to %s. Run: ekctl login --url %s", base, base)
	}
	return a.newClient(base, token, cfg.TrustedCerts[hostPort(base)]), nil
}

func (c *client) url(path string) string {
	if strings.HasPrefix(path, "/api/") {
		return c.base + path
	}
	return c.base + "/api/v1/" + strings.TrimPrefix(path, "/")
}

// do sends a request. On an error answer it returns the body as well as an *APIError.
func (c *client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return nil, usagef("Cannot make the request: %v", err)
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &connError{base: c.base, err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, &connError{base: c.base, err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return data, apiErrorFrom(resp.StatusCode, data)
	}
	return data, nil
}

// call sends in (if not nil) as JSON and decodes the answer into out (if not nil). It
// returns the raw answer for --json.
func (c *client) call(ctx context.Context, method, path string, in, out any) ([]byte, error) {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return nil, err
		}
	}
	raw, err := c.do(ctx, method, path, body)
	if err != nil {
		return raw, err
	}
	if out != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("The panel's answer to %s %s is not what ekctl expected (%v). Check the panel URL, or update ekctl.", method, c.url(path), err)
		}
	}
	return raw, nil
}

func (c *client) get(ctx context.Context, path string, out any) ([]byte, error) {
	return c.call(ctx, http.MethodGet, path, nil, out)
}

// connError is a request that got no answer.
type connError struct {
	base string
	err  error
}

func (e *connError) Unwrap() error { return e.err }

func (e *connError) Error() string {
	var pm *pinMismatchError
	var cve *tls.CertificateVerificationError
	switch {
	case errors.As(e.err, &pm):
		return fmt.Sprintf("The certificate of %s is not the one you trusted. It has the SHA-256 fingerprint %s. "+
			"If the panel has a new certificate, check it and trust it with: ekctl login --url %s", e.base, pm.got, e.base)
	case errors.As(e.err, &cve):
		return fmt.Sprintf("The certificate of %s is not trusted (%v). Check it and trust it with: ekctl login --url %s", e.base, cve.Err, e.base)
	}
	inner := e.err
	var ue *url.Error
	if errors.As(inner, &ue) {
		inner = ue.Err
	}
	return fmt.Sprintf("Cannot reach %s: %v", e.base, inner)
}

// untrustedCert returns the certificate a connection failed on, when it failed because
// the certificate is not trusted (or not the trusted one).
func untrustedCert(err error) (cert *x509.Certificate, reason string, changed bool) {
	var pm *pinMismatchError
	if errors.As(err, &pm) {
		return pm.cert, "it is not the certificate you trusted before", true
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) && len(cve.UnverifiedCertificates) > 0 {
		return cve.UnverifiedCertificates[0], cve.Err.Error(), false
	}
	return nil, "", false
}

// pinMismatchError: the panel presented a certificate that is neither trusted by the
// system nor the one the user pinned.
type pinMismatchError struct {
	want, got string
	cert      *x509.Certificate
}

func (e *pinMismatchError) Error() string {
	return "certificate fingerprint " + e.got + " is not the trusted " + e.want
}

// pinnedTLS accepts a certificate the system trusts for the host, or exactly the leaf
// certificate with the pinned SHA-256 fingerprint. Go's own check is replaced, not
// skipped: VerifyConnection runs on every connection, resumed ones included.
func pinnedTLS(pin string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // VerifyConnection below does the checking
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the panel sent no certificate")
			}
			leaf := cs.PeerCertificates[0]
			opts := x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: x509.NewCertPool()}
			for _, c := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(c)
			}
			if _, err := leaf.Verify(opts); err == nil {
				return nil
			}
			if got := fingerprint(leaf.Raw); got != pin {
				return &pinMismatchError{want: pin, got: got, cert: leaf}
			}
			return nil
		},
	}
}

// fingerprint is the SHA-256 of a DER certificate as AB:CD:….
func fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return formatFingerprint(sum[:])
}

func formatFingerprint(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":")
}

// normalizeFingerprint accepts a SHA-256 fingerprint with or without colons, in any case.
func normalizeFingerprint(s string) (string, error) {
	clean := strings.NewReplacer(":", "", " ", "", "-", "").Replace(strings.TrimSpace(s))
	b, err := hex.DecodeString(clean)
	if err != nil || len(b) != sha256.Size {
		return "", usagef("%q is not a SHA-256 fingerprint. It has 64 hex digits, such as AB:CD:… (32 pairs).", s)
	}
	return formatFingerprint(b), nil
}
