package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/go-chi/chi/v5"
)

// The API description (OpenAPI 3.1) is made from the router itself: every route, with a
// summary from its handler's name (or summaryOverrides), a tag from its first path segment,
// its path parameters, and the scope an API token needs (tokenScopeFor). A route cannot be
// added without appearing here, and docs/openapi.json in the repository must match
// (TestOpenAPIFile).

// summaryOverrides replaces summaries that do not read well when made from a handler name.
var summaryOverrides = map[string]string{
	"GET /api/v1/health":                                "Check that the panel is running (and its version)",
	"GET /api/v1/branding":                              "The panel's name, color and logo for you (or, signed out, for the domain it was opened at)",
	"GET /api/v1/branding/logo/{id}":                    "A brand's logo (PNG)",
	"GET /api/v1/brand":                                 "Your white-label settings (the panel's, for administrators)",
	"PUT /api/v1/brand":                                 "Set your panel's name, color and domain",
	"PUT /api/v1/brand/logo":                            "Upload your panel's logo (PNG, JPEG, GIF or WebP, sent as application/octet-stream)",
	"DELETE /api/v1/brand/logo":                         "Remove your panel's logo",
	"GET /api/v1/auth/me":                               "The signed-in user",
	"GET /api/v1/me/usage":                              "Your usage against your package's limits",
	"POST /api/v1/hooks/git/{id}":                       "Webhook from a Git host: deploy an app (signed with the app's webhook secret)",
	"GET /api/v1/openapi.json":                          "This API description",
	"GET /api/v1/php-versions":                          "Supported PHP versions",
	"GET /api/v1/php-apps":                              "PHP apps that can be installed (Joomla, Drupal, Moodle, Laravel)",
	"GET /api/v1/catalog":                               "The one-click app catalogue",
	"GET /api/v1/audit":                                 "The audit log",
	"GET /api/v1/updates":                               "Panel and agent versions, and the newest release",
	"POST /api/v1/updates/check":                        "Look for a new release now",
	"POST /api/v1/updates/panel":                        "Update the panel (then every agent) to the newest release",
	"POST /api/v1/updates/agents":                       "Update every agent to the panel's version",
	"POST /api/v1/sites/{id}/files":                     "File manager action (list, read, write, move, delete …)",
	"GET /api/v1/sites/{id}/files/download":             "Download a file",
	"POST /api/v1/sites/{id}/files/upload":              "Upload a file",
	"POST /api/v1/apps/{id}/deploy":                     "Deploy a Git app now",
	"POST /api/v1/apps/{id}/deployments/{did}/rollback": "Roll back to this deployment",
	"POST /api/v1/notifications/read":                   "Mark notifications as read",
	"POST /api/v1/customers/{id}/impersonate":           "Sign in as this customer",
	"POST /api/v1/auth/confirm":                         "Confirm your password (for terminals)",
	"POST /api/v1/setup":                                "Create the first administrator (needs the setup token)",
	"DELETE /api/v1/tokens/{id}":                        "Revoke a token",
	"PUT /api/v1/sites/{id}/wordpress/auto":             "Set WordPress automatic updates",
	"POST /api/v1/sites/{id}/wordpress/copy":            "Copy between a site and its staging copy",
	"POST /api/v1/dns/zones/{id}/ssl":                   "Set the zone's Cloudflare SSL mode",
	"POST /api/v1/imports/inspect":                      "Read a cPanel backup on a server: its websites, databases and cron jobs",
	"POST /api/v1/imports":                              "Import websites, databases, email, cron jobs and DNS zones from a cPanel backup",
	"GET /api/v1/imports/files":                         "The backups and archives in a server's import folder",
	"POST /api/v1/imports/site/inspect":                 "Read a website archive (any host, aaPanel) on a server",
	"POST /api/v1/imports/site":                         "Import a website, and its database, from an archive",
	"POST /api/v1/imports/coolify/inspect":              "List the applications, services and databases of a Coolify instance",
	"POST /api/v1/imports/coolify":                      "Make apps of Coolify applications and services",
	"POST /api/v1/auth/passkey/options":                 "Start signing in with a passkey",
	"POST /api/v1/auth/passkey/login":                   "Sign in with a passkey",
	"POST /api/v1/auth/passkeys/options":                "Start adding a passkey (asks for the password)",
	"GET /api/v1/auth/2fa/recovery-codes":               "How many two-factor recovery codes are left",
	"POST /api/v1/auth/2fa/recovery-codes":              "Make new two-factor recovery codes",
	"POST /api/v1/customers/{id}/2fa/reset":             "Turn off a customer's two-factor sign-in",
}

// tagNames gives a few path segments a nicer tag.
var tagNames = map[string]string{
	"auth": "account", "me": "account", "tokens": "account", "setup": "account",
	"wordpress": "sites", "php-apps": "sites", "php-versions": "sites",
	"catalog": "apps", "packages": "customers", "hooks": "apps", "health": "panel",
	"updates": "settings", "audit": "settings", "openapi.json": "panel",
}

// publicRoutes need no sign-in.
var publicRoutes = []string{
	"GET /api/v1/health", "GET /api/v1/openapi.json", "GET /api/v1/setup/status", "POST /api/v1/setup",
	"POST /api/v1/auth/login", "POST /api/v1/hooks/git/{id}",
}

// splitCamel splits "TOTPDisableCFAccounts" into TOTP, Disable, CF, Accounts.
func splitCamel(s string) []string {
	r := []rune(s)
	var words []string
	start := 0
	for i := 1; i < len(r); i++ {
		lowerBefore := unicode.IsLower(r[i-1]) || unicode.IsDigit(r[i-1])
		acronymEnd := unicode.IsUpper(r[i-1]) && unicode.IsUpper(r[i]) && i+1 < len(r) && unicode.IsLower(r[i+1])
		if unicode.IsUpper(r[i]) && (lowerBefore || acronymEnd) {
			words = append(words, string(r[start:i]))
			start = i
		}
	}
	if start < len(r) {
		words = append(words, string(r[start:]))
	}
	return words
}

// acronyms keep their case in summaries.
var acronyms = map[string]string{"dns": "DNS", "php": "PHP", "totp": "two-factor", "sftp": "SFTP", "ftp": "FTP",
	"ip": "IP", "ssl": "SSL", "api": "API", "url": "URL", "id": "ID", "wp": "WordPress", "2fa": "two-factor", "db": "database",
	"cf": "Cloudflare", "ssh": "SSH"}

// summaryFromHandler turns "handleCreateSiteAlias" into "Create site alias".
func summaryFromHandler(name string) string {
	name = name[strings.LastIndex(name, ".")+1:]
	name = strings.TrimSuffix(name, "-fm")
	name = strings.TrimPrefix(name, "handle")
	words := splitCamel(name)
	for i, w := range words {
		lw := strings.ToLower(w)
		if a, ok := acronyms[lw]; ok {
			words[i] = a
			continue
		}
		words[i] = lw
	}
	s := strings.Join(words, " ")
	s = strings.ReplaceAll(s, "word press", "WordPress")
	s = strings.ReplaceAll(s, "cloudflare", "Cloudflare")
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

func handlerName(h http.Handler) string {
	if ch, ok := h.(*chi.ChainHandler); ok {
		h = ch.Endpoint
	}
	v := reflect.ValueOf(h)
	if v.Kind() != reflect.Func {
		return ""
	}
	if f := runtime.FuncForPC(v.Pointer()); f != nil {
		return f.Name()
	}
	return ""
}

type apiRoute struct {
	Method, Path, Summary, Tag, Scope string
	Public                            bool
}

// apiRoutes lists the API's routes from the router, sorted.
func (s *Server) apiRoutes() ([]apiRoute, error) {
	var out []apiRoute
	err := chi.Walk(s.router(), func(method, route string, h http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		route = strings.TrimSuffix(route, "/")
		key := method + " " + route
		summary, ok := summaryOverrides[key]
		if !ok {
			summary = summaryFromHandler(handlerName(h))
		}
		seg := strings.SplitN(strings.TrimPrefix(route, "/api/v1/"), "/", 2)[0]
		tag := seg
		if t, ok := tagNames[seg]; ok {
			tag = t
		}
		out = append(out, apiRoute{Method: method, Path: route, Summary: summary, Tag: tag,
			Scope: TokenScopeFor(method, route), Public: slices.Contains(publicRoutes, key)})
		return nil
	})
	slices.SortFunc(out, func(a, b apiRoute) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		return strings.Compare(a.Method, b.Method)
	})
	return out, err
}

var pathParamRE = regexp.MustCompile(`\{([a-zA-Z_]+)(?::[^}]*)?\}`)

// OpenAPI returns the API description as JSON.
func (s *Server) OpenAPI() ([]byte, error) {
	routes, err := s.apiRoutes()
	if err != nil {
		return nil, err
	}
	errorResponse := map[string]any{"description": "An error", "content": map[string]any{"application/json": map[string]any{
		"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}}
	paths := map[string]map[string]any{}
	for _, r := range routes {
		path := pathParamRE.ReplaceAllString(r.Path, "{$1}")
		op := map[string]any{
			"summary":     r.Summary,
			"operationId": operationID(r.Method, path),
			"tags":        []string{r.Tag},
			"responses": map[string]any{
				"2XX":     map[string]any{"description": "Success (JSON, or no content)"},
				"default": errorResponse,
			},
		}
		var params []any
		for _, m := range pathParamRE.FindAllStringSubmatch(r.Path, -1) {
			params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
		}
		if params != nil {
			op["parameters"] = params
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch {
			op["requestBody"] = map[string]any{"required": false, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
		}
		switch {
		case r.Public:
			op["security"] = []any{}
		case r.Scope == "":
			op["security"] = []any{map[string]any{"session": []string{}}}
			op["x-ekpanel-token"] = "API tokens cannot use this; it needs a signed-in session"
		default:
			op["x-ekpanel-scope"] = r.Scope
		}
		if paths[path] == nil {
			paths[path] = map[string]any{}
		}
		paths[path][strings.ToLower(r.Method)] = op
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "ekPanel API",
			"version": "1",
			"description": "The API behind the ekPanel web interface. Authenticate with an API token (Account → API tokens) " +
				"sent as `Authorization: Bearer ekp_…`. A token acts as its owner, limited by its scopes; `x-ekpanel-scope` " +
				"on each operation names the scope it needs. Errors are JSON: `{\"code\": \"…\", \"message\": \"…\"}`. " +
				"Request and response schemas are not described yet.",
			"license": map[string]any{"name": "AGPL-3.0-only", "identifier": "AGPL-3.0-only"},
		},
		"servers":  []any{map[string]any{"url": "/", "description": "This panel"}},
		"security": []any{map[string]any{"token": []string{}}, map[string]any{"session": []string{}}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"token":   map[string]any{"type": "http", "scheme": "bearer", "description": "An API token: ekp_…"},
				"session": map[string]any{"type": "apiKey", "in": "cookie", "name": "__Host-ekpanel_session", "description": "The web interface's session cookie"},
			},
			"schemas": map[string]any{
				"Error": map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{
					"code":    map[string]any{"type": "string", "description": "Stable; the interface translates it"},
					"message": map[string]any{"type": "string"},
				}},
			},
		},
		"paths": paths,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// operationID makes "getSitesId"-style IDs: method plus the path's words.
func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, part := range strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/") {
		part = strings.Trim(part, "{}")
		for _, w := range regexp.MustCompile(`[^a-zA-Z0-9]+`).Split(part, -1) {
			if w == "" {
				continue
			}
			b.WriteString(strings.ToUpper(w[:1]) + w[1:])
		}
	}
	return b.String()
}

var (
	openapiOnce sync.Once
	openapiJSON []byte
	openapiErr  error
)

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	openapiOnce.Do(func() { openapiJSON, openapiErr = s.OpenAPI() })
	if openapiErr != nil {
		s.internalError(w, r, openapiErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(openapiJSON)
}
