package api

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// API tokens let scripts and the ekctl CLI use the API. A token acts as its owner, limited
// by its scopes. See PLAN.md (4b).

const tokenPrefix = "ekp_"

// Scopes a token can have. "read" allows every GET its owner may see; the others allow
// changes (and reads) in one area.
var (
	TokenScopes    = []string{"read", "sites", "databases", "apps", "dns", "mail", "backups", "servers", "customers", "settings"}
	customerScopes = []string{"read", "sites", "databases", "apps", "dns", "mail", "settings"}
	resellerScopes = []string{"read", "sites", "databases", "apps", "dns", "mail", "customers", "settings"}
)

// routeScopes maps the first segment of an API path to the scopes that allow it. A segment
// that is not here cannot be used with a token at all.
var routeScopes = map[string][]string{
	"sites":         {"sites"},
	"wordpress":     {"sites"},
	"moves":         {"sites", "apps", "databases"},
	"imports":       {"sites"},
	"php-apps":      {"sites"},
	"php-versions":  {"sites"},
	"cron":          {"sites", "apps"},
	"apps":          {"apps"},
	"catalog":       {"apps"},
	"databases":     {"databases"},
	"dns":           {"dns"},
	"mail":          {"mail"},
	"backups":       {"backups"},
	"servers":       {"servers"},
	"customers":     {"customers"},
	"packages":      {"customers"},
	"owner-changes": {"customers"},
	"resellers":     {"customers"},
	"settings":      {"settings"},
	"brand":         {"settings"},
	"updates":       {"settings"},
	"audit":         {"settings"},
	"notifications": {"settings"},
	"tasks":         {},
	"me":            {},
	// Public reads: a client that always sends its token must still reach them.
	"health":       {},
	"openapi.json": {},
	"setup":        {},
	"branding":     {},
}

// tokenScopeFor returns the scopes that allow a request with a token, and whether a token
// may make it at all. An empty list means any token may read it (only GET): who the token
// belongs to, and the tasks it started.
func tokenScopeFor(method, path string) (scopes []string, ok bool) {
	rest := strings.TrimPrefix(path, "/api/v1/")
	parts := strings.Split(rest, "/")
	// Never with a token: account security, tokens themselves, and terminals (a root shell).
	if parts[0] == "auth" {
		return nil, method == http.MethodGet && rest == "auth/me"
	}
	// Nor anything that makes a browser session: signing in as a customer would give a
	// session without the token's limits.
	if slices.Contains(parts, "terminal") || slices.Contains(parts, "impersonate") || slices.Contains(parts, "2fa") {
		return nil, false
	}
	scopes, ok = routeScopes[parts[0]]
	if !ok {
		return nil, false
	}
	if len(scopes) == 0 && method != http.MethodGet {
		return nil, false
	}
	return scopes, true
}

// TokenScopeFor is tokenScopeFor for the API description: the scope a route needs for a
// change, or "read".
func TokenScopeFor(method, path string) string {
	scopes, ok := tokenScopeFor(method, path)
	switch {
	case !ok:
		return ""
	case method == http.MethodGet && len(scopes) == 0:
		return "any"
	case method == http.MethodGet:
		return "read or " + strings.Join(scopes, " or ")
	}
	return strings.Join(scopes, " or ")
}

func currentToken(r *http.Request) *store.APIToken {
	t, _ := r.Context().Value(tokenKey).(*store.APIToken)
	return t
}

// tokenAuth reads a bearer token. It returns false after writing an error.
func (s *Server) tokenAuth(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return r, true
	}
	raw, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || !strings.HasPrefix(raw, tokenPrefix) {
		writeError(w, http.StatusUnauthorized, "invalid_token", "send an API token as: Authorization: Bearer ekp_…")
		return r, false
	}
	u, t, err := s.store.TokenUser(r.Context(), auth.HashToken(raw), clientIP(r))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid_token", "this API token does not exist, was revoked or has expired")
		return r, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return r, false
	}
	if u.SuspendedAt != nil {
		writeSuspended(w)
		return r, false
	}
	scopes, allowed := tokenScopeFor(r.Method, r.URL.Path)
	if !allowed {
		writeError(w, http.StatusForbidden, "token_not_allowed", "this needs a signed-in session; API tokens cannot do it")
		return r, false
	}
	has := len(scopes) == 0 || slices.ContainsFunc(scopes, func(sc string) bool { return slices.Contains(t.Scopes, sc) })
	if !has && !(r.Method == http.MethodGet && slices.Contains(t.Scopes, "read")) {
		need := strings.Join(scopes, " or ")
		if r.Method == http.MethodGet {
			need = strings.Join(append([]string{"read"}, scopes...), " or ")
		}
		writeError(w, http.StatusForbidden, "token_scope", "this API token does not have the scope for this: "+need)
		return r, false
	}
	ctx := context.WithValue(r.Context(), userKey, &u)
	return r.WithContext(context.WithValue(ctx, tokenKey, &t)), true
}

// ---- Handlers (signed-in sessions only) ----

type createTokenRequest struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days"` // 0: never
}

type createdToken struct {
	store.APIToken
	// Token is shown only this once.
	Token string `json:"token"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListAPITokens(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var req createTokenRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	name := strings.TrimSpace(req.Name)
	if name == "" || len([]rune(name)) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "enter a name of up to 100 characters")
		return
	}
	allowed := TokenScopes
	switch u.Role {
	case "customer":
		allowed = customerScopes
	case "reseller":
		allowed = resellerScopes
	}
	scopes := []string{}
	for _, sc := range req.Scopes {
		if !slices.Contains(allowed, sc) {
			writeError(w, http.StatusBadRequest, "invalid_scope", "unknown scope: "+sc)
			return
		}
		if !slices.Contains(scopes, sc) {
			scopes = append(scopes, sc)
		}
	}
	if len(scopes) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_scope", "choose at least one scope")
		return
	}
	slices.SortFunc(scopes, func(a, b string) int { return slices.Index(TokenScopes, a) - slices.Index(TokenScopes, b) })
	var expires *time.Time
	switch req.ExpiresInDays {
	case 0:
	case 30, 90, 365:
		e := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour)
		expires = &e
	default:
		writeError(w, http.StatusBadRequest, "invalid_expiry", "expiry is 30, 90 or 365 days, or never")
		return
	}
	if n, err := s.store.CountAPITokens(r.Context(), u.ID); err != nil {
		s.internalError(w, r, err)
		return
	} else if n >= store.MaxTokensPerUser {
		writeError(w, http.StatusConflict, "too_many_tokens", "revoke a token you no longer use first")
		return
	}
	raw := tokenPrefix + auth.NewToken()
	t, err := s.store.CreateAPIToken(r.Context(), u.ID, name, raw[:len(tokenPrefix)+6], auth.HashToken(raw), scopes, expires)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "token.created", t.Name, map[string]any{"token_id": t.ID, "scopes": t.Scopes, "expires_at": t.ExpiresAt})
	writeJSON(w, http.StatusCreated, createdToken{APIToken: t, Token: raw})
}

func (s *Server) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !isUUID(id) {
		writeError(w, http.StatusNotFound, "token_not_found", "no such token")
		return
	}
	t, err := s.store.DeleteAPIToken(r.Context(), currentUser(r).ID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "token_not_found", "no such token")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "token.revoked", t.Name, map[string]any{"token_id": t.ID})
	w.WriteHeader(http.StatusNoContent)
}

// withToken adds the token to an audit entry's details.
func withToken(r *http.Request, details map[string]any) map[string]any {
	t := currentToken(r)
	if t == nil {
		return details
	}
	details = maps.Clone(details)
	if details == nil {
		details = map[string]any{}
	}
	details["via_token"] = t.Name
	details["token_id"] = t.ID
	return details
}
