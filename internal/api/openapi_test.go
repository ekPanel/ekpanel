package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"ekpanel/internal/api"
	"ekpanel/internal/hub"
)

// TestOpenAPIFile checks the API description: every route has a summary, and
// docs/openapi.json matches. After changing routes: UPDATE_OPENAPI=1 go test ./internal/api -run TestOpenAPIFile
func TestOpenAPIFile(t *testing.T) {
	s := api.New(nil, hub.New(), slog.New(slog.NewTextHandler(io.Discard, nil)), api.Options{})
	got, err := s.OpenAPI()
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Summary string `json:"summary"`
			Scope   string `json:"x-ekpanel-scope"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for path, ops := range doc.Paths {
		for method, op := range ops {
			n++
			if len(op.Summary) < 4 {
				t.Errorf("%s %s has no summary; name its handler well or add it to summaryOverrides", method, path)
			}
		}
	}
	if n < 150 {
		t.Fatalf("only %d operations described", n)
	}
	if sites := doc.Paths["/api/v1/sites"]; sites["get"].Scope != "read or sites" || sites["post"].Scope != "sites" {
		t.Fatalf("scopes of /sites: %+v", sites)
	}

	file := filepath.Join("..", "..", "docs", "openapi.json")
	got = append(got, '\n')
	if os.Getenv("UPDATE_OPENAPI") != "" {
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, _ := os.ReadFile(file)
	if !bytes.Equal(got, want) {
		t.Fatal("docs/openapi.json is out of date; run: UPDATE_OPENAPI=1 go test ./internal/api -run TestOpenAPIFile")
	}
}

func TestOpenAPIServed(t *testing.T) {
	srv := newPanel(t)
	resp, err := http.Get(srv.URL + "/api/v1/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc map[string]any
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&doc) != nil || doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi.json: %d %v", resp.StatusCode, doc["openapi"])
	}
}
