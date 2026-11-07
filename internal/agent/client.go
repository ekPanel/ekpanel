package agent

import (
	"bytes"
	"context"
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

	"hostpanel/internal/agentproto"
)

// Config is what enrolment writes to disk and what `run` reads back.
type Config struct {
	PanelURL string `json:"panel_url"`
	ServerID string `json:"server_id"`
	Secret   string `json:"secret"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read agent config (run `hostpanel-agent enroll` first): %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if c.PanelURL == "" || c.ServerID == "" || c.Secret == "" {
		return c, fmt.Errorf("%s is incomplete; enrol the agent again", path)
	}
	return c, nil
}

// Enroll trades a one-time token for this server's permanent credential and saves it.
func Enroll(ctx context.Context, panelURL, token, configPath string) (Config, error) {
	panelURL = strings.TrimRight(panelURL, "/")
	body, _ := json.Marshal(agentproto.EnrollRequest{Token: token})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, panelURL+agentproto.EnrollPath, bytes.NewReader(body))
	if err != nil {
		return Config{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Config{}, fmt.Errorf("contact panel: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return Config{}, fmt.Errorf("panel refused enrolment (%s): %s", resp.Status, e.Message)
	}
	var out agentproto.EnrollResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Config{}, fmt.Errorf("read enrolment response: %w", err)
	}
	cfg := Config{PanelURL: panelURL, ServerID: out.ServerID, Secret: out.Secret}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return Config{}, err
	}
	if err := os.WriteFile(configPath, b, 0o600); err != nil {
		return Config{}, fmt.Errorf("save credential: %w", err)
	}
	return cfg, nil
}

// Run keeps a connection to the panel open, reconnecting with backoff, until ctx ends.
func Run(ctx context.Context, log *slog.Logger, cfg Config, version string) error {
	collector := NewCollector("/proc", "/")
	backoff := time.Second
	for {
		started := time.Now()
		err := session(ctx, log, cfg, collector, version)
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
	url := strings.Replace(cfg.PanelURL, "http", "ws", 1) + agentproto.ConnectPath
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + cfg.ServerID + "." + cfg.Secret}},
	})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errors.New("panel rejected this agent's credential; it may have been re-enrolled or deleted")
		}
		return err
	}
	defer conn.CloseNow()
	// The panel sends nothing yet, but reading keeps ping/close frames flowing.
	ctx = conn.CloseRead(ctx)
	log.Info("connected to panel", "url", cfg.PanelURL)

	if err := send(ctx, conn, agentproto.Envelope{Type: agentproto.TypeHello, Hello: ptr(Facts(version))}); err != nil {
		return err
	}
	tick := time.NewTicker(agentproto.MetricsInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "agent stopping")
			return ctx.Err()
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
