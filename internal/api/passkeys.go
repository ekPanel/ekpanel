package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// Passkeys (WebAuthn). A passkey is a discoverable credential with user verification, so
// signing in with one counts as both factors. The relying party is the panel's host name as
// the browser reaches it: a passkey made on one address does not work on another.

// ceremonyTTL is how long the browser has to answer a passkey request.
const ceremonyTTL = 5 * time.Minute

type ceremony struct {
	data   webauthn.SessionData
	userID string // registration: the account adding a passkey
	rpID   string
	until  time.Time
}

// ceremonies keeps the challenges handed out, by a random id the browser sends back.
type ceremonies struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func (c *ceremonies) put(x ceremony) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]ceremony{}
	}
	now := time.Now()
	for k, v := range c.m {
		if now.After(v.until) {
			delete(c.m, k)
		}
	}
	id := auth.NewToken()
	x.until = now.Add(ceremonyTTL)
	c.m[id] = x
	return id
}

// take returns and forgets a ceremony: each challenge is answered once.
func (c *ceremonies) take(id string) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, ok := c.m[id]
	delete(c.m, id)
	return x, ok && time.Now().Before(x.until)
}

// webAuthnFor sets up WebAuthn for the address this request came to.
func webAuthnFor(r *http.Request) (*webauthn.WebAuthn, string, error) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return nil, "", errors.New("passkeys need the panel's host name, not an address")
	}
	scheme := "https"
	if r.TLS == nil && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		scheme = "http" // browsers allow WebAuthn on http://localhost only
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          host,
		RPDisplayName: "ekPanel",
		RPOrigins:     []string{scheme + "://" + r.Host},
	})
	return wa, host, err
}

// passkeyUser is an account as go-webauthn sees it.
type passkeyUser struct {
	u      store.User
	handle []byte
	creds  []webauthn.Credential
}

func (p *passkeyUser) WebAuthnID() []byte                         { return p.handle }
func (p *passkeyUser) WebAuthnName() string                       { return p.u.Email }
func (p *passkeyUser) WebAuthnDisplayName() string                { return p.u.Name }
func (p *passkeyUser) WebAuthnCredentials() []webauthn.Credential { return p.creds }

func (s *Server) loadPasskeyUser(ctx context.Context, u store.User) (*passkeyUser, error) {
	handle, err := s.store.WebAuthnHandle(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	keys, err := s.store.ListPasskeys(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	pu := &passkeyUser{u: u, handle: handle}
	for _, k := range keys {
		var c webauthn.Credential
		if json.Unmarshal(k.Credential, &c) == nil {
			pu.creds = append(pu.creds, c)
		}
	}
	return pu, nil
}

func (s *Server) writePasskeyUnavailable(w http.ResponseWriter, err error) {
	writeErrorData(w, http.StatusBadRequest, "passkey_unavailable", "passkeys cannot be used on this address: "+err.Error(),
		map[string]any{"detail": err.Error()})
}

// ---- Adding, listing and removing ----

func (s *Server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.ListPasskeys(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if keys == nil {
		keys = []store.Passkey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// handlePasskeyOptions starts adding a passkey; the password is asked again.
func (s *Server) handlePasskeyOptions(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	if !s.checkPasswordAgain(w, u, req.Password) {
		return
	}
	wa, rpID, err := webAuthnFor(r)
	if err != nil {
		s.writePasskeyUnavailable(w, err)
		return
	}
	pu, err := s.loadPasskeyUser(r.Context(), *u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range pu.creds {
		exclude = append(exclude, c.Descriptor())
	}
	options, data, err := wa.BeginRegistration(pu,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey: protocol.ResidentKeyRequirementRequired, RequireResidentKey: protocol.ResidentKeyRequired(),
			UserVerification: protocol.VerificationRequired}),
		webauthn.WithExclusions(exclude),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	id := s.passkeyCeremonies.put(ceremony{data: *data, userID: u.ID, rpID: rpID})
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": options})
}

func (s *Server) handleAddPasskey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ceremony   string          `json:"ceremony"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	c, ok := s.passkeyCeremonies.take(req.Ceremony)
	if !ok || c.userID != u.ID {
		writeError(w, http.StatusBadRequest, "passkey_expired", "the request took too long; try again")
		return
	}
	wa, rpID, err := webAuthnFor(r)
	if err != nil || rpID != c.rpID {
		writeError(w, http.StatusBadRequest, "passkey_failed", "the passkey could not be added")
		return
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(req.Credential)
	if err != nil {
		writeError(w, http.StatusBadRequest, "passkey_failed", "the passkey could not be added")
		return
	}
	pu, err := s.loadPasskeyUser(r.Context(), *u)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cred, err := wa.CreateCredential(pu, c.data, parsed)
	if err != nil {
		s.log.Info("passkey not added", "user", u.Email, "err", err)
		writeError(w, http.StatusBadRequest, "passkey_failed", "the passkey could not be added")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Passkey"
	}
	if len([]rune(name)) > 60 {
		name = string([]rune(name)[:60])
	}
	raw, _ := json.Marshal(cred)
	key, err := s.store.AddPasskey(r.Context(), u.ID, name, cred.ID, raw)
	if errors.Is(err, store.ErrPasskeyExists) {
		writeError(w, http.StatusConflict, "passkey_exists", "this passkey is added already")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.passkey_added", u.Email, map[string]any{"passkey_id": key.ID, "name": name})
	writeJSON(w, http.StatusCreated, key)
}

func (s *Server) handleDeletePasskey(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := chi.URLParam(r, "id")
	ok, err := s.store.DeletePasskey(r.Context(), u.ID, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "passkey_not_found", "no such passkey")
		return
	}
	s.audit(r, "auth.passkey_removed", u.Email, map[string]any{"passkey_id": id})
	w.WriteHeader(http.StatusNoContent)
}

// ---- Signing in ----

func (s *Server) handlePasskeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	if s.limiter.Blocked("ip:" + clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again in 15 minutes")
		return
	}
	wa, rpID, err := webAuthnFor(r)
	if err != nil {
		s.writePasskeyUnavailable(w, err)
		return
	}
	options, data, err := wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	id := s.passkeyCeremonies.put(ceremony{data: *data, rpID: rpID})
	writeJSON(w, http.StatusOK, map[string]any{"ceremony": id, "options": options})
}

func (s *Server) handlePasskeyLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ceremony   string          `json:"ceremony"`
		Credential json.RawMessage `json:"credential"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	ipKey := "ip:" + clientIP(r)
	if s.limiter.Blocked(ipKey) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again in 15 minutes")
		return
	}
	fail := func(err error) {
		s.limiter.Fail(ipKey)
		s.auditAs(r, nil, "auth.login_failed", "", map[string]any{"reason": "passkey", "detail": err.Error()})
		writeError(w, http.StatusUnauthorized, "passkey_failed", "the passkey could not sign you in")
	}
	c, ok := s.passkeyCeremonies.take(req.Ceremony)
	if !ok || c.userID != "" {
		writeError(w, http.StatusBadRequest, "passkey_expired", "the request took too long; try again")
		return
	}
	wa, rpID, err := webAuthnFor(r)
	if err != nil || rpID != c.rpID {
		fail(errors.New("another address"))
		return
	}
	parsed, err := protocol.ParseCredentialRequestResponseBytes(req.Credential)
	if err != nil {
		fail(err)
		return
	}
	var found *passkeyUser
	cred, err := wa.ValidateDiscoverableLogin(func(rawID, handle []byte) (webauthn.User, error) {
		u, err := s.store.UserByWebAuthnHandle(r.Context(), handle)
		if err != nil {
			return nil, err
		}
		found, err = s.loadPasskeyUser(r.Context(), u)
		return found, err
	}, c.data, parsed)
	if err != nil || found == nil {
		if err == nil {
			err = errors.New("no account")
		}
		fail(err)
		return
	}
	if cred.Authenticator.CloneWarning {
		fail(errors.New("the authenticator's counter went back: it may be cloned"))
		return
	}
	raw, _ := json.Marshal(cred)
	if err := s.store.UsePasskey(r.Context(), cred.ID, raw); err != nil {
		s.internalError(w, r, err)
		return
	}
	u := found.u
	token := auth.NewToken()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateSessionBy(r.Context(), auth.HashToken(token), u.ID, clientIP(r), ua, "passkey"); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setSessionCookie(w, token, int(store.SessionTTL.Seconds()))
	s.checkNewSignIn(r, &u)
	s.auditAs(r, &u.ID, "auth.login", u.Email, map[string]any{"method": "passkey"})
	writeJSON(w, http.StatusOK, u)
}
