package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"image/png"
	"maps"
	"net/http"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/pquerna/otp"

	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

type ctxKey int

const (
	userKey ctxKey = iota
	sessionHashKey
	tokenKey
)

func (s *Server) cookieName() string {
	if s.opts.SecureCookies {
		// The __Host- prefix makes browsers refuse the cookie unless it is Secure, host-only and Path=/.
		return "__Host-ekpanel_session"
	}
	return "ekpanel_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.opts.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	})
}

// loadSession attaches the signed-in user (if any) to the request context.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An API token, when sent, decides alone: no cookie is looked at.
		if r.Header.Get("Authorization") != "" {
			r, ok := s.tokenAuth(w, r)
			if ok {
				next.ServeHTTP(w, r)
			}
			return
		}
		c, err := r.Cookie(s.cookieName())
		if err != nil || c.Value == "" {
			next.ServeHTTP(w, r)
			return
		}
		hash := auth.HashToken(c.Value)
		u, err := s.store.SessionUser(r.Context(), hash)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.setSessionCookie(w, "", -1)
		case err != nil:
			s.internalError(w, r, err)
			return
		default:
			ctx := context.WithValue(r.Context(), userKey, &u)
			r = r.WithContext(context.WithValue(ctx, sessionHashKey, hash))
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(userKey).(*store.User)
	return u
}

func requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func requireRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return requireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !slices.Contains(roles, currentUser(r).Role) {
				writeError(w, http.StatusForbidden, "forbidden", "you do not have permission for this")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

func notImpersonating(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r).ImpersonatorID != nil {
			writeError(w, http.StatusForbidden, "forbidden", "only the account's owner can do this")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) audit(r *http.Request, action, target string, details map[string]any) {
	var actor *string
	if u := currentUser(r); u != nil {
		actor = &u.ID
		if u.ImpersonatorID != nil {
			// The administrator did it, in the customer's account.
			actor = u.ImpersonatorID
			details = maps.Clone(details)
			if details == nil {
				details = map[string]any{}
			}
			details["as_customer"] = u.ID
		}
	}
	s.auditAs(r, actor, action, target, withToken(r, details))
}

func (s *Server) auditAs(r *http.Request, actorID *string, action, target string, details map[string]any) {
	// Audit writes must not be lost because the client hung up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := s.store.AddAudit(ctx, actorID, clientIP(r), action, target, details); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

// ---- Setup ----

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	has, err := s.store.HasUsers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"needs_setup": !has})
}

type setupRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
	Locale   string `json:"locale"`
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var req setupRequest
	if !readJSON(w, r, &req) {
		return
	}
	ip := clientIP(r)
	if s.limiter.Blocked("setup:" + ip) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; try again later")
		return
	}

	s.setupMu.Lock()
	want := s.setupToken
	s.setupMu.Unlock()
	if want == "" {
		writeError(w, http.StatusConflict, "already_set_up", "this panel already has an administrator")
		return
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(req.Token)), []byte(want)) != 1 {
		s.limiter.Fail("setup:" + ip)
		writeError(w, http.StatusForbidden, "invalid_setup_token", "the setup token is not correct")
		return
	}
	email, name, ok := validateAccount(w, req.Email, req.Name, req.Password)
	if !ok {
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	u, err := s.store.CreateFirstAdmin(r.Context(), email, name, hash, normalizeLocale(req.Locale))
	if errors.Is(err, store.ErrAlreadySetUp) {
		writeError(w, http.StatusConflict, "already_set_up", "this panel already has an administrator")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	s.setupMu.Lock()
	s.setupToken = ""
	s.setupMu.Unlock()
	if s.opts.OnSetupDone != nil {
		s.opts.OnSetupDone()
	}
	s.auditAs(r, &u.ID, "setup.admin_created", u.Email, nil)

	if err := s.startSession(w, r, &u); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func validateAccount(w http.ResponseWriter, email, name, password string) (string, string, bool) {
	email = strings.TrimSpace(email)
	name = strings.TrimSpace(name)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid email address")
		return "", "", false
	}
	if name == "" || len(name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "enter a name of up to 100 characters")
		return "", "", false
	}
	if len([]rune(password)) < auth.MinPasswordLength || len(password) > 1024 {
		writeError(w, http.StatusBadRequest, "weak_password", "use a password of at least 10 characters")
		return "", "", false
	}
	return email, name, true
}

// supportedLocales lists the UI languages with a translation file. Keep in sync with web/src/i18n.
var supportedLocales = map[string]bool{"en": true, "bn": true}

func normalizeLocale(l string) string {
	if supportedLocales[l] {
		return l
	}
	return "en"
}

// ---- Login / logout ----

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !readJSON(w, r, &req) {
		return
	}
	ipKey := "ip:" + clientIP(r)
	emailKey := "email:" + strings.ToLower(strings.TrimSpace(req.Email))
	if s.limiter.Blocked(ipKey) || s.limiter.Blocked(emailKey) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed sign-ins; try again in 15 minutes")
		return
	}
	fail := func(code, message string) {
		s.limiter.Fail(ipKey)
		s.limiter.Fail(emailKey)
		s.auditAs(r, nil, "auth.login_failed", strings.TrimSpace(req.Email), map[string]any{"reason": code})
		writeError(w, http.StatusUnauthorized, code, message)
	}

	u, err := s.store.UserByEmail(r.Context(), strings.TrimSpace(req.Email))
	if errors.Is(err, store.ErrNotFound) {
		auth.BurnPasswordCheck(req.Password)
		fail("invalid_credentials", "email or password is not correct")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if ok, err := auth.VerifyPassword(req.Password, u.PasswordHash); err != nil || !ok {
		fail("invalid_credentials", "email or password is not correct")
		return
	}
	if u.TOTPEnabled {
		if req.TOTPCode == "" {
			// Not a failure: the password was right and the client should ask for the code.
			writeError(w, http.StatusUnauthorized, "totp_required", "enter the code from your authenticator app")
			return
		}
		if u.TOTPSecret == nil || !auth.CheckTOTP(*u.TOTPSecret, strings.TrimSpace(req.TOTPCode)) {
			fail("invalid_totp", "the authentication code is not correct")
			return
		}
	}

	s.limiter.Reset(emailKey)
	if err := s.startSession(w, r, &u); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.checkNewSignIn(r, &u)
	s.auditAs(r, &u.ID, "auth.login", u.Email, nil)
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) error {
	token := auth.NewToken()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateSession(r.Context(), auth.HashToken(token), u.ID, clientIP(r), ua); err != nil {
		return err
	}
	s.setSessionCookie(w, token, int(store.SessionTTL.Seconds()))
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	if err := s.store.DeleteSession(r.Context(), hash); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.setSessionCookie(w, "", -1)
	s.audit(r, "auth.logout", currentUser(r).Email, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

type updateMeRequest struct {
	Locale *string `json:"locale"`
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	if req.Locale != nil {
		if !supportedLocales[*req.Locale] {
			writeError(w, http.StatusBadRequest, "unsupported_locale", "that language is not available")
			return
		}
		if err := s.store.SetUserLocale(r.Context(), u.ID, *req.Locale); err != nil {
			s.internalError(w, r, err)
			return
		}
		u.Locale = *req.Locale
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	u := currentUser(r)
	if err := s.store.DeleteOtherSessions(r.Context(), u.ID, hash); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.sessions_revoked", u.Email, nil)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Two-factor authentication ----

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.TOTPEnabled {
		writeError(w, http.StatusConflict, "totp_already_enabled", "two-factor authentication is already on")
		return
	}
	secret, url, err := auth.NewTOTP("ekPanel", u.Email)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	qr, err := qrDataURL(url)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Stored as pending; it only takes effect once a valid code confirms it.
	if err := s.store.SetTOTP(r.Context(), u.ID, &secret, false); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"secret": secret, "otpauth_url": url, "qr_png": qr})
}

func qrDataURL(otpauthURL string) (string, error) {
	key, err := otp.NewKeyFromURL(otpauthURL)
	if err != nil {
		return "", err
	}
	img, err := key.Image(240, 240)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

type codeRequest struct {
	Code string `json:"code"`
}

func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var req codeRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	if u.TOTPEnabled {
		writeError(w, http.StatusConflict, "totp_already_enabled", "two-factor authentication is already on")
		return
	}
	if u.TOTPSecret == nil {
		writeError(w, http.StatusConflict, "totp_not_started", "start two-factor setup first")
		return
	}
	if !auth.CheckTOTP(*u.TOTPSecret, strings.TrimSpace(req.Code)) {
		writeError(w, http.StatusBadRequest, "invalid_totp", "the authentication code is not correct")
		return
	}
	if err := s.store.SetTOTP(r.Context(), u.ID, u.TOTPSecret, true); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.2fa_enabled", u.Email, nil)
	u.TOTPEnabled = true
	writeJSON(w, http.StatusOK, u)
}

type passwordRequest struct {
	Password string `json:"password"`
}

func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	key := "reauth:" + u.ID
	if s.limiter.Blocked(key) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; try again later")
		return
	}
	if ok, err := auth.VerifyPassword(req.Password, u.PasswordHash); err != nil || !ok {
		s.limiter.Fail(key)
		writeError(w, http.StatusForbidden, "invalid_password", "the password is not correct")
		return
	}
	if err := s.store.SetTOTP(r.Context(), u.ID, nil, false); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.2fa_disabled", u.Email, nil)
	u.TOTPEnabled = false
	writeJSON(w, http.StatusOK, u)
}
