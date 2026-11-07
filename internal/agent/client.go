package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/pki"
)

// Config is what enrolment writes to disk (mode 0600) and what `run` reads back.
type Config struct {
	// Panel is the panel's agent address, host:port.
	Panel    string `json:"panel"`
	ServerID string `json:"server_id"`
	CAPEM    string `json:"ca_pem"`
	CertPEM  string `json:"cert_pem"`
	KeyPEM   string `json:"key_pem"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read agent config (run `ekpanel-agent enroll` first): %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.Panel == "" || c.ServerID == "" || c.CAPEM == "" || c.CertPEM == "" || c.KeyPEM == "" {
		return c, fmt.Errorf("%s is incomplete or from an older agent; enrol the agent again", path)
	}
	return c, nil
}

// save writes the config atomically, so a crash mid-write never leaves a broken credential.
func (c Config) save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalizeAddr accepts "host", "host:port" or a URL and returns host:port.
func normalizeAddr(panel string) string {
	panel = strings.TrimSpace(panel)
	if _, rest, ok := strings.Cut(panel, "://"); ok {
		panel = rest
	}
	panel = strings.TrimRight(strings.SplitN(panel, "/", 2)[0], "/")
	if !strings.Contains(panel, ":") || strings.HasSuffix(panel, "]") {
		panel = fmt.Sprintf("%s:%d", panel, agentproto.DefaultPort)
	}
	return panel
}

func postJSON(ctx context.Context, client *http.Client, url string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("contact panel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("panel refused the request (%s): %s", resp.Status, e.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func httpClient(cfg *tls.Config) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, ForceAttemptHTTP2: true}}
}

// Enroll trades a one-time token for this server's certificate and saves the credential.
// pin is the panel CA's fingerprint ("sha256:…"), shown next to the token in the panel.
func Enroll(ctx context.Context, panel, token, pin, configPath string) (Config, error) {
	addr := normalizeAddr(panel)
	keyPEM, csrPEM, err := pki.NewAgentKey()
	if err != nil {
		return Config{}, err
	}
	var caPEM []byte
	var out agentproto.EnrollResponse
	err = postJSON(ctx, httpClient(pki.PinnedTLSConfig(pin, &caPEM)), "https://"+addr+agentproto.EnrollPath,
		agentproto.EnrollRequest{Token: strings.TrimSpace(token), CSRPEM: string(csrPEM)}, &out)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{Panel: addr, ServerID: out.ServerID, CAPEM: string(caPEM), CertPEM: out.CertPEM, KeyPEM: string(keyPEM)}
	if _, err := cfg.TLSConfig(); err != nil {
		return Config{}, fmt.Errorf("panel returned an unusable certificate: %w", err)
	}
	if err := cfg.save(configPath); err != nil {
		return Config{}, fmt.Errorf("save credential: %w", err)
	}
	return cfg, nil
}

// TLSConfig is the agent's mutual-TLS client configuration.
func (c Config) TLSConfig() (*tls.Config, error) {
	return pki.AgentTLSConfig([]byte(c.CAPEM), []byte(c.CertPEM), []byte(c.KeyPEM))
}

// renewIfDue replaces the agent certificate when it is close to expiry. The new key is
// created here and never leaves the server.
func renewIfDue(ctx context.Context, log *slog.Logger, cfg *Config, configPath string) error {
	cert, err := pki.ParseCertPEM([]byte(cfg.CertPEM))
	if err != nil {
		return err
	}
	if time.Until(cert.NotAfter) > pki.RenewBefore {
		return nil
	}
	tlsCfg, err := cfg.TLSConfig()
	if err != nil {
		return err
	}
	keyPEM, csrPEM, err := pki.NewAgentKey()
	if err != nil {
		return err
	}
	var out agentproto.RenewResponse
	if err := postJSON(ctx, httpClient(tlsCfg), "https://"+cfg.Panel+agentproto.RenewPath,
		agentproto.RenewRequest{CSRPEM: string(csrPEM)}, &out); err != nil {
		return fmt.Errorf("renew certificate: %w", err)
	}
	next := *cfg
	next.CertPEM, next.KeyPEM = out.CertPEM, string(keyPEM)
	if _, err := next.TLSConfig(); err != nil {
		return fmt.Errorf("renewed certificate unusable: %w", err)
	}
	if err := next.save(configPath); err != nil {
		return err
	}
	*cfg = next
	log.Info("renewed agent certificate", "expires", cert.NotAfter)
	return nil
}

// Run keeps a connection to the panel open, reconnecting with backoff, until ctx ends.
func Run(ctx context.Context, log *slog.Logger, cfg Config, configPath, version string) error {
	collector := NewCollector("/proc", "/")
	backoff := time.Second
	for {
		started := time.Now()
		err := renewIfDue(ctx, log, &cfg, configPath)
		if err == nil {
			err = session(ctx, log, cfg, collector, version)
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // the connection was healthy for a while
		}
		wait := backoff + rand.N(backoff/2+1)
		log.Warn("lost connection to panel; retrying", "err", err, "in", wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func session(ctx context.Context, log *slog.Logger, cfg Config, collector *Collector, version string) error {
	tlsCfg, err := cfg.TLSConfig()
	if err != nil {
		return err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, "wss://"+cfg.Panel+agentproto.ConnectPath, &websocket.DialOptions{
		HTTPClient: httpClient(tlsCfg),
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errors.New("panel rejected this agent's certificate; it may have been re-enrolled or deleted")
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()
	log.Info("connected to panel", "panel", cfg.Panel)

	if err := send(ctx, conn, agentproto.Envelope{Type: agentproto.TypeHello, Hello: ptr(Facts(version))}); err != nil {
		return err
	}

	// Requests from the panel run concurrently; each answers with a response envelope.
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var env agentproto.Envelope
			if json.Unmarshal(data, &env) != nil || env.Type != agentproto.TypeRequest {
				continue
			}
			go func(req agentproto.Envelope) {
				// Installing PHP takes minutes; backing up a large site can take hours.
				opCtx, cancel := context.WithTimeout(ctx, 12*time.Hour)
				defer cancel()
				resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: req.ID}
				result, opErr := Dispatch(opCtx, req.Op, req.Params)
				if opErr != nil {
					resp.Error = opErr
					log.Warn("operation failed", "op", req.Op, "err", opErr)
				} else {
					resp.Result, _ = json.Marshal(result)
				}
				send(ctx, conn, resp)
			}(env)
		}
	}()

	tick := time.NewTicker(agentproto.MetricsInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "agent stopping")
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-tick.C:
			m, err := collector.Sample()
			if err != nil {
				log.Error("collect metrics", "err", err)
				continue
			}
			if err := send(ctx, conn, agentproto.Envelope{Type: agentproto.TypeMetrics, Metrics: &m}); err != nil {
				return err
			}
		}
	}
}

// send is safe to call from several goroutines: websocket writes are serialized internally.
func send(ctx context.Context, conn *websocket.Conn, env agentproto.Envelope) error {
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

func ptr[T any](v T) *T { return &v }
