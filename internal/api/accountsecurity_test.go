package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestRecoveryCodesAndSessions(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)

	var setup struct {
		Secret string `json:"secret"`
	}
	c.do("POST", "/api/v1/auth/2fa/setup", nil, &setup)
	code, _ := totp.GenerateCode(setup.Secret, time.Now())
	var enabled struct {
		TOTPEnabled   bool     `json:"totp_enabled"`
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if st := c.do("POST", "/api/v1/auth/2fa/enable", map[string]string{"code": code}, &enabled); st != http.StatusOK || !enabled.TOTPEnabled || len(enabled.RecoveryCodes) != 10 {
		t.Fatalf("enable: %d %+v", st, enabled)
	}
	var left struct {
		Left int `json:"left"`
	}
	c.do("GET", "/api/v1/auth/2fa/recovery-codes", nil, &left)
	if left.Left != 10 {
		t.Fatalf("left %d", left.Left)
	}

	// A recovery code signs in once, however it is typed.
	other := newClient(t, p.web.URL)
	typed := strings.ToUpper(strings.ReplaceAll(enabled.RecoveryCodes[0], "-", " "))
	login := map[string]string{"email": "admin@example.com", "password": "a-long-password", "totp_code": typed}
	if st := other.do("POST", "/api/v1/auth/login", login, nil); st != http.StatusOK {
		t.Fatalf("sign in with a recovery code: %d", st)
	}
	third := newClient(t, p.web.URL)
	var e errBody
	if st := third.do("POST", "/api/v1/auth/login", login, &e); st != http.StatusUnauthorized || e.Code != "invalid_totp" {
		t.Fatalf("a used code: %d %s", st, e.Code)
	}
	c.do("GET", "/api/v1/auth/2fa/recovery-codes", nil, &left)
	if left.Left != 9 {
		t.Fatalf("left after one %d", left.Left)
	}
	// New codes need the password, and replace the old ones.
	if st := c.do("POST", "/api/v1/auth/2fa/recovery-codes", map[string]string{"password": "wrong"}, &e); st != http.StatusForbidden {
		t.Fatalf("new codes with a wrong password: %d", st)
	}
	var fresh struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if st := c.do("POST", "/api/v1/auth/2fa/recovery-codes", map[string]string{"password": "a-long-password"}, &fresh); st != http.StatusOK || len(fresh.RecoveryCodes) != 10 {
		t.Fatalf("new codes: %d", st)
	}
	login["totp_code"] = enabled.RecoveryCodes[1]
	if st := third.do("POST", "/api/v1/auth/login", login, &e); st != http.StatusUnauthorized {
		t.Fatalf("an old code after new ones: %d", st)
	}

	// Sessions: two browsers; this one is marked and cannot be ended here; the other can.
	var sessions []struct {
		ID      string `json:"id"`
		Method  string `json:"method"`
		Current bool   `json:"current"`
	}
	c.do("GET", "/api/v1/auth/sessions", nil, &sessions)
	if len(sessions) != 2 || sessions[0].Method != "password" {
		t.Fatalf("sessions %+v", sessions)
	}
	var mine, theirs string
	for _, s := range sessions {
		if s.Current {
			mine = s.ID
		} else {
			theirs = s.ID
		}
	}
	if mine == "" || theirs == "" {
		t.Fatalf("sessions %+v", sessions)
	}
	if st := c.do("DELETE", "/api/v1/auth/sessions/"+mine, nil, &e); st != http.StatusBadRequest || e.Code != "session_current" {
		t.Fatalf("end this session: %d %s", st, e.Code)
	}
	if st := c.do("DELETE", "/api/v1/auth/sessions/"+theirs, nil, nil); st != http.StatusNoContent {
		t.Fatalf("end the other: %d", st)
	}
	if st := other.do("GET", "/api/v1/auth/me", nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("the ended session still works: %d", st)
	}
	if st := c.do("DELETE", "/api/v1/auth/sessions/"+theirs, nil, &e); st != http.StatusNotFound {
		t.Fatalf("end it again: %d", st)
	}

	// Turning two-factor off removes the codes.
	c.do("POST", "/api/v1/auth/2fa/disable", map[string]string{"password": "a-long-password"}, nil)
	c.do("GET", "/api/v1/auth/2fa/recovery-codes", nil, &left)
	if left.Left != 0 {
		t.Fatalf("codes left after turning off %d", left.Left)
	}
}

func TestResetCustomerTwoFactor(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	var pkg struct {
		ID string `json:"id"`
	}
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_sites": 1}, &pkg)
	var created struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &created)
	cust := login(t, p.web.URL, "c@example.com", created.Password)
	var setup struct {
		Secret string `json:"secret"`
	}
	cust.do("POST", "/api/v1/auth/2fa/setup", nil, &setup)
	code, _ := totp.GenerateCode(setup.Secret, time.Now())
	cust.do("POST", "/api/v1/auth/2fa/enable", map[string]string{"code": code}, nil)

	var e errBody
	if st := cust.do("POST", "/api/v1/customers/"+created.Customer.ID+"/2fa/reset", nil, &e); st != http.StatusForbidden {
		t.Fatalf("a customer resets two-factor: %d", st)
	}
	if st := admin.do("POST", "/api/v1/customers/"+created.Customer.ID+"/2fa/reset", nil, nil); st != http.StatusNoContent {
		t.Fatalf("reset: %d", st)
	}
	fresh := newClient(t, p.web.URL)
	if st := fresh.do("POST", "/api/v1/auth/login", map[string]string{"email": "c@example.com", "password": created.Password}, &e); st != http.StatusOK {
		t.Fatalf("sign in after the reset: %d %s", st, e.Code)
	}
	var adminMe struct {
		ID string `json:"id"`
	}
	admin.do("GET", "/api/v1/auth/me", nil, &adminMe)
	if st := admin.do("POST", "/api/v1/customers/"+adminMe.ID+"/2fa/reset", nil, &e); st != http.StatusNotFound {
		t.Fatalf("reset an administrator from the customers page: %d", st)
	}
}
