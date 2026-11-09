// Package catalog holds the one-click services: Compose files reviewed by the project,
// with the variables each needs. Installing one makes an ordinary Compose app whose
// secrets are generated on the spot.
//
// Rules for a template (checked by the tests, and by the agent for customers' apps):
// named volumes only (no folders of the host), no published ports (the panel publishes the
// web port on 127.0.0.1), no privileged mode, extra capabilities, devices, host network or
// Docker socket. Values that differ per install come from ${VARIABLES} listed in Vars.
package catalog

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"slices"
	"strings"
)

//go:embed templates/*.yaml
var files embed.FS

// Var is one variable of a template. Exactly one of Generate and Value is set.
type Var struct {
	Name string `json:"name"`
	// Generate makes a random value at install: "password" (24 letters and digits) or
	// "hex" (64 hex digits, for keys).
	Generate string `json:"generate,omitempty"`
	// Value is fixed text, where {domain} is the app's domain. Values that use the domain
	// follow it when the domain changes.
	Value string `json:"value,omitempty"`
	// Login marks values the user needs to sign in; the app page shows them.
	Login bool `json:"login,omitempty"`
}

// Template is one service of the catalogue. Its description and notes are translated in
// the web UI (catalog.apps.<id>), so they are not here.
type Template struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Website  string `json:"website"`
	// Version goes up whenever the Compose file or variables change.
	Version int `json:"version"`
	// Service and Port receive the app's domain. Port 0 means nothing is published.
	Service string `json:"service"`
	Port    int    `json:"port,omitempty"`
	// NeedsDomain is true for web apps, which are reachable only through their domain.
	NeedsDomain bool     `json:"needs_domain"`
	Images      []string `json:"images"`
	MemoryMB    int      `json:"memory_mb"` // what it needs to run comfortably
	Vars        []Var    `json:"vars"`
	Compose     string   `json:"compose"`
}

var templates = []Template{
	{ID: "uptime-kuma", Name: "Uptime Kuma", Category: "monitoring", Website: "https://uptime.kuma.pet", Version: 1,
		Service: "uptime-kuma", Port: 3001, NeedsDomain: true, Images: []string{"louislam/uptime-kuma:2"}, MemoryMB: 256},
	{ID: "grafana", Name: "Grafana", Category: "monitoring", Website: "https://grafana.com/oss/grafana/", Version: 1,
		Service: "grafana", Port: 3000, NeedsDomain: true, Images: []string{"grafana/grafana:latest"}, MemoryMB: 256,
		Vars: []Var{
			{Name: "ADMIN_USER", Value: "admin", Login: true},
			{Name: "ADMIN_PASSWORD", Generate: "password", Login: true},
			{Name: "ROOT_URL", Value: "https://{domain}/"},
		}},
	{ID: "n8n", Name: "n8n", Category: "automation", Website: "https://n8n.io", Version: 1,
		Service: "n8n", Port: 5678, NeedsDomain: true, Images: []string{"docker.n8n.io/n8nio/n8n:latest"}, MemoryMB: 512,
		Vars: []Var{
			{Name: "N8N_HOST", Value: "{domain}"},
			{Name: "WEBHOOK_URL", Value: "https://{domain}/"},
			{Name: "N8N_ENCRYPTION_KEY", Generate: "hex"},
		}},
	{ID: "gitea", Name: "Gitea", Category: "development", Website: "https://about.gitea.com", Version: 1,
		Service: "gitea", Port: 3000, NeedsDomain: true, Images: []string{"gitea/gitea:1"}, MemoryMB: 256,
		Vars: []Var{
			{Name: "DOMAIN", Value: "{domain}"},
			{Name: "ROOT_URL", Value: "https://{domain}/"},
		}},
	{ID: "it-tools", Name: "IT Tools", Category: "development", Website: "https://it-tools.tech", Version: 1,
		Service: "it-tools", Port: 80, NeedsDomain: true, Images: []string{"corentinth/it-tools:latest"}, MemoryMB: 64},
	{ID: "vaultwarden", Name: "Vaultwarden", Category: "security", Website: "https://github.com/dani-garcia/vaultwarden", Version: 1,
		Service: "vaultwarden", Port: 80, NeedsDomain: true, Images: []string{"vaultwarden/server:latest"}, MemoryMB: 128,
		Vars: []Var{
			{Name: "DOMAIN_URL", Value: "https://{domain}"},
			{Name: "SIGNUPS_ALLOWED", Value: "true"},
			{Name: "ADMIN_TOKEN", Generate: "password", Login: true},
		}},
	{ID: "umami", Name: "Umami", Category: "analytics", Website: "https://umami.is", Version: 1,
		Service: "umami", Port: 3000, NeedsDomain: true, Images: []string{"ghcr.io/umami-software/umami:postgresql-latest", "postgres:17-alpine"}, MemoryMB: 512,
		Vars: []Var{
			{Name: "DB_PASSWORD", Generate: "password"},
			{Name: "APP_SECRET", Generate: "hex"},
		}},
	{ID: "ghost", Name: "Ghost", Category: "publishing", Website: "https://ghost.org", Version: 1,
		Service: "ghost", Port: 2368, NeedsDomain: true, Images: []string{"ghost:6-alpine", "mysql:8.4"}, MemoryMB: 768,
		Vars: []Var{
			{Name: "URL", Value: "https://{domain}"},
			{Name: "DB_PASSWORD", Generate: "password"},
		}},
	{ID: "listmonk", Name: "listmonk", Category: "publishing", Website: "https://listmonk.app", Version: 1,
		Service: "listmonk", Port: 9000, NeedsDomain: true, Images: []string{"listmonk/listmonk:latest", "postgres:17-alpine"}, MemoryMB: 256,
		Vars: []Var{
			{Name: "ADMIN_USER", Value: "admin", Login: true},
			{Name: "ADMIN_PASSWORD", Generate: "password", Login: true},
			{Name: "DB_PASSWORD", Generate: "password"},
		}},
	{ID: "nextcloud", Name: "Nextcloud", Category: "productivity", Website: "https://nextcloud.com", Version: 1,
		Service: "nextcloud", Port: 80, NeedsDomain: true, Images: []string{"nextcloud:32-apache", "postgres:17-alpine"}, MemoryMB: 1024,
		Vars: []Var{
			{Name: "DOMAIN", Value: "{domain}"},
			{Name: "ADMIN_USER", Value: "admin", Login: true},
			{Name: "ADMIN_PASSWORD", Generate: "password", Login: true},
			{Name: "DB_PASSWORD", Generate: "password"},
		}},
	{ID: "memos", Name: "Memos", Category: "productivity", Website: "https://usememos.com", Version: 1,
		Service: "memos", Port: 5230, NeedsDomain: true, Images: []string{"neosmemo/memos:stable"}, MemoryMB: 128},
	{ID: "excalidraw", Name: "Excalidraw", Category: "productivity", Website: "https://excalidraw.com", Version: 1,
		Service: "excalidraw", Port: 80, NeedsDomain: true, Images: []string{"excalidraw/excalidraw:latest"}, MemoryMB: 64},
	{ID: "ntfy", Name: "ntfy", Category: "notifications", Website: "https://ntfy.sh", Version: 1,
		Service: "ntfy", Port: 80, NeedsDomain: true, Images: []string{"binwiederhier/ntfy:latest"}, MemoryMB: 64,
		Vars: []Var{
			{Name: "BASE_URL", Value: "https://{domain}"},
		}},
	{ID: "redis", Name: "Redis", Category: "database", Website: "https://redis.io", Version: 1,
		Service: "redis", Port: 6379, Images: []string{"redis:8-alpine"}, MemoryMB: 128,
		Vars: []Var{
			{Name: "REDIS_PASSWORD", Generate: "password", Login: true},
		}},
}

func init() {
	for i := range templates {
		b, err := files.ReadFile("templates/" + templates[i].ID + ".yaml")
		if err != nil {
			panic("catalog: no Compose file for " + templates[i].ID)
		}
		templates[i].Compose = string(b)
		if templates[i].Vars == nil {
			templates[i].Vars = []Var{}
		}
	}
}

// All returns every template, in catalogue order.
func All() []Template { return slices.Clone(templates) }

// Get returns the template with that ID.
func Get(id string) (Template, bool) {
	for _, t := range templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// Env returns the variables of a new install on the given domain, with fresh secrets.
func (t Template) Env(domain string) []Variable {
	env := make([]Variable, 0, len(t.Vars))
	for _, v := range t.Vars {
		value := v.Value
		switch v.Generate {
		case "password":
			value = randomPassword()
		case "hex":
			value = randomHex()
		default:
			value = strings.ReplaceAll(value, "{domain}", domain)
		}
		env = append(env, Variable{Key: v.Name, Value: value})
	}
	return env
}

// FollowDomain updates, in place, the variables whose value is made from the domain.
func (t Template) FollowDomain(env []Variable, domain string) {
	for _, v := range t.Vars {
		if v.Generate != "" || !strings.Contains(v.Value, "{domain}") {
			continue
		}
		for i := range env {
			if env[i].Key == v.Name {
				env[i].Value = strings.ReplaceAll(v.Value, "{domain}", domain)
			}
		}
	}
}

// Variable is a name and value, the same shape as an app's environment variable.
type Variable struct {
	Key   string
	Value string
}

const passwordChars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// randomPassword has no symbols, so it is safe in URLs, shell commands and config files.
func randomPassword() string {
	b := make([]byte, 24)
	for i := range b {
		b[i] = passwordChars[randIndex(len(passwordChars))]
	}
	return string(b)
}

func randIndex(n int) int {
	// Rejection sampling keeps every character equally likely.
	limit := 256 - 256%n
	var c [1]byte
	for {
		rand.Read(c[:])
		if int(c[0]) < limit {
			return int(c[0]) % n
		}
	}
}

func randomHex() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
