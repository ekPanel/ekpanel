// Package config loads panel settings from flags and EKPANEL_* environment variables.
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	// Listen is the address the HTTP server (web UI and API) binds to.
	Listen string
	// AgentListen is the mutual-TLS listener agents connect to.
	AgentListen string
	// DatabaseURL is a PostgreSQL connection string. Empty means DevDB must be set.
	DatabaseURL string
	// DevDB starts a private embedded PostgreSQL inside DataDir (development only).
	DevDB bool
	// DataDir holds panel state that is not in the database (setup token, dev database).
	DataDir string
	// SecureCookies marks session cookies Secure. Turn off only for plain-HTTP local development.
	SecureCookies bool
	// ReleasesURL is where releases are published (GitHub's layout); a mirror may replace it.
	ReleasesURL string
	// UpdateCheck lets the panel look for new releases every few hours.
	UpdateCheck bool
}

// Load parses args (without the program name) on top of environment defaults.
func Load(args []string) (*Config, error) {
	c := &Config{
		Listen:        env("EKPANEL_LISTEN", "127.0.0.1:8080"),
		AgentListen:   env("EKPANEL_AGENT_LISTEN", ":7443"),
		DatabaseURL:   env("EKPANEL_DATABASE_URL", ""),
		DevDB:         envBool("EKPANEL_DEV_DB", false),
		DataDir:       env("EKPANEL_DATA_DIR", "/var/lib/ekpanel"),
		SecureCookies: envBool("EKPANEL_SECURE_COOKIES", true),
		ReleasesURL:   env("EKPANEL_RELEASES_URL", "https://github.com/ekPanel/ekpanel/releases"),
		UpdateCheck:   envBool("EKPANEL_UPDATE_CHECK", true),
	}

	fs := flag.NewFlagSet("ekpanel", flag.ContinueOnError)
	fs.StringVar(&c.Listen, "listen", c.Listen, "address for the web UI and API")
	fs.StringVar(&c.AgentListen, "agent-listen", c.AgentListen, "address agents connect to (mutual TLS)")
	fs.StringVar(&c.DatabaseURL, "database-url", c.DatabaseURL, "PostgreSQL connection URL")
	fs.BoolVar(&c.DevDB, "dev-db", c.DevDB, "run an embedded PostgreSQL in the data dir (development only)")
	fs.StringVar(&c.DataDir, "data-dir", c.DataDir, "directory for panel state")
	fs.BoolVar(&c.SecureCookies, "secure-cookies", c.SecureCookies, "set the Secure flag on session cookies")
	fs.StringVar(&c.ReleasesURL, "releases-url", c.ReleasesURL, "where ekPanel releases are published")
	fs.BoolVar(&c.UpdateCheck, "update-check", c.UpdateCheck, "look for new ekPanel releases every few hours")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	if c.DatabaseURL == "" && !c.DevDB {
		return nil, fmt.Errorf("no database configured: set --database-url or use --dev-db for development")
	}
	abs, err := filepath.Abs(c.DataDir)
	if err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	c.DataDir = abs
	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
