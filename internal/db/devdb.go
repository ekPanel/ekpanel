package db

import (
	"fmt"
	"io"
	"net"
	"path/filepath"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// DevDB is a private PostgreSQL server for development and tests. It downloads PostgreSQL
// binaries on first use (into cacheDir, or dir/cache when empty) and keeps its data under dir.
// Never use it in production.
type DevDB struct {
	pg  *embeddedpostgres.EmbeddedPostgres
	URL string
}

func StartDevDB(dir, cacheDir string, logs io.Writer) (*DevDB, error) {
	if cacheDir == "" {
		cacheDir = filepath.Join(dir, "cache")
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Port(uint32(port)).
		Database("hostpanel").
		Username("hostpanel").
		Password("hostpanel").
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		BinariesPath(filepath.Join(dir, "bin")).
		CachePath(cacheDir).
		Logger(logs)
	pg := embeddedpostgres.NewDatabase(cfg)
	if err := pg.Start(); err != nil {
		return nil, fmt.Errorf("start embedded postgres: %w", err)
	}
	url := fmt.Sprintf("postgres://hostpanel:hostpanel@127.0.0.1:%d/hostpanel?sslmode=disable", port)
	return &DevDB{pg: pg, URL: url}, nil
}

func (d *DevDB) Stop() error { return d.pg.Stop() }

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
