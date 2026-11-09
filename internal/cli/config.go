package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Config is the saved login. It holds a token, so it is written readable by its owner only.
type Config struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
	// TrustedCerts maps a panel's host:port to the SHA-256 fingerprint of the certificate
	// its user chose to trust (a self-signed one).
	TrustedCerts map[string]string `json:"trusted_certs,omitempty"`
}

func (a *app) configPath() (string, error) {
	if a.g.Config != "" {
		return a.g.Config, nil
	}
	if p := a.env.Getenv("EKCTL_CONFIG"); p != "" {
		return p, nil
	}
	if a.env.ConfigDir == "" {
		return "", errors.New("Cannot find your config folder. Choose a config file with --config or EKCTL_CONFIG.")
	}
	return filepath.Join(a.env.ConfigDir, "ekctl", "config.json"), nil
}

// config loads the config file once. A missing file is an empty config.
func (a *app) config() (*Config, error) {
	if a.cfg != nil {
		return a.cfg, nil
	}
	path, err := a.configPath()
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("Cannot read the config file: %v", err)
	default:
		if err := json.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("The config file %s is not valid JSON (%v). Fix it, or delete it and log in again.", path, err)
		}
	}
	if cfg.TrustedCerts == nil {
		cfg.TrustedCerts = map[string]string{}
	}
	a.cfg, a.cfgPath = cfg, path
	return cfg, nil
}

// saveConfig writes the config atomically: folder 0700, file 0600.
func (a *app) saveConfig() error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(a.cfgPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("Cannot create the config folder: %v", err)
	}
	f, err := os.CreateTemp(dir, ".config-*.json") // created 0600
	if err != nil {
		return fmt.Errorf("Cannot write the config file: %v", err)
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		f.Close()
		return fmt.Errorf("Cannot write the config file: %v", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("Cannot write the config file: %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("Cannot write the config file: %v", err)
	}
	if err := os.Rename(f.Name(), a.cfgPath); err != nil {
		return fmt.Errorf("Cannot write the config file: %v", err)
	}
	return nil
}

// normalizeURL turns what a person types into the panel's base URL: https:// is added when
// no scheme is given, and a trailing slash or /api/v1 is removed.
func normalizeURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("No panel URL.")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q is not a panel URL. Use one like https://panel.example.com", raw)
	}
	p := strings.TrimRight(u.Path, "/")
	p = strings.TrimRight(strings.TrimSuffix(p, "/api/v1"), "/")
	return u.Scheme + "://" + strings.ToLower(u.Host) + p, nil
}

// hostPort is the key of a panel's trusted certificate.
func hostPort(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	return strings.ToLower(u.Hostname()) + ":" + port
}
