package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
)

func TestPasskeys(t *testing.T) {
	p := newTestPanel(t)
	// Passkeys need a host name: reach the test server as localhost.
	base := strings.Replace(p.web.URL, "127.0.0.1", "localhost", 1)
	c := newClient(t, base)
	setUp(t, c)
	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "ekPanel", Origin: base}

	var e errBody
	if st := c.do("POST", "/api/v1/auth/passkeys/options", map[string]string{"password": "wrong"}, &e); st != http.StatusForbidden {
		t.Fatalf("options with a wrong password: %d", st)
	}
	var reg struct {
		Ceremony string          `json:"ceremony"`
		Options  json.RawMessage `json:"options"`
	}
	if st := c.do("POST", "/api/v1/auth/passkeys/options", map[string]string{"password": "a-long-password"}, &reg); st != http.StatusOK || reg.Ceremony == "" {
		t.Fatalf("options: %d", st)
	}
	att, err := virtualwebauthn.ParseAttestationOptions(string(reg.Options))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(reg.Options), `"residentKey":"required"`) || !strings.Contains(string(reg.Options), `"userVerification":"required"`) {
		t.Fatalf("options do not ask for a discoverable, verified passkey: %s", reg.Options)
	}
	authn := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte(att.UserID)})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	resp := virtualwebauthn.CreateAttestationResponse(rp, authn, cred, *att)
	var key struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if st := c.do("POST", "/api/v1/auth/passkeys", map[string]any{"ceremony": reg.Ceremony, "name": "Laptop", "credential": json.RawMessage(resp)}, &key); st != http.StatusCreated || key.Name != "Laptop" {
		t.Fatalf("add: %d %+v", st, key)
	}
	authn.AddCredential(cred)
	// A ceremony is answered once.
	if st := c.do("POST", "/api/v1/auth/passkeys", map[string]any{"ceremony": reg.Ceremony, "credential": json.RawMessage(resp)}, &e); st != http.StatusBadRequest || e.Code != "passkey_expired" {
		t.Fatalf("the same ceremony again: %d %s", st, e.Code)
	}

	// Signing in with it: no password, no code, a session marked passkey.
	fresh := newClient(t, base)
	signIn := func(cl *client) int {
		var opt struct {
			Ceremony string          `json:"ceremony"`
			Options  json.RawMessage `json:"options"`
		}
		if st := cl.do("POST", "/api/v1/auth/passkey/options", nil, &opt); st != http.StatusOK {
			t.Fatalf("login options: %d", st)
		}
		as, err := virtualwebauthn.ParseAssertionOptions(string(opt.Options))
		if err != nil {
			t.Fatal(err)
		}
		r := virtualwebauthn.CreateAssertionResponse(rp, authn, cred, *as)
		return cl.do("POST", "/api/v1/auth/passkey/login", map[string]any{"ceremony": opt.Ceremony, "credential": json.RawMessage(r)}, &e)
	}
	if st := signIn(fresh); st != http.StatusOK {
		t.Fatalf("passkey sign-in: %d %s", st, e.Code)
	}
	var me struct {
		Email string `json:"email"`
	}
	if fresh.do("GET", "/api/v1/auth/me", nil, &me); me.Email != "admin@example.com" {
		t.Fatalf("signed in as %q", me.Email)
	}
	var sessions []struct {
		Method  string `json:"method"`
		Current bool   `json:"current"`
	}
	fresh.do("GET", "/api/v1/auth/sessions", nil, &sessions)
	found := false
	for _, s := range sessions {
		found = found || s.Current && s.Method == "passkey"
	}
	if !found {
		t.Fatalf("sessions %+v", sessions)
	}
	var keys []struct {
		ID         string  `json:"id"`
		LastUsedAt *string `json:"last_used_at"`
	}
	c.do("GET", "/api/v1/auth/passkeys", nil, &keys)
	if len(keys) != 1 || keys[0].LastUsedAt == nil {
		t.Fatalf("passkeys %+v", keys)
	}

	// Reached by another name, the passkey does not work; removed, it signs no one in.
	other := newClient(t, p.web.URL)
	if st := other.do("POST", "/api/v1/auth/passkey/options", nil, &e); st != http.StatusBadRequest || e.Code != "passkey_unavailable" {
		t.Fatalf("by address: %d %s", st, e.Code)
	}
	if st := c.do("DELETE", "/api/v1/auth/passkeys/"+keys[0].ID, nil, nil); st != http.StatusNoContent {
		t.Fatalf("remove: %d", st)
	}
	if st := signIn(newClient(t, base)); st != http.StatusUnauthorized || e.Code != "passkey_failed" {
		t.Fatalf("a removed passkey: %d %s", st, e.Code)
	}
}
