package api

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// ---- Two-factor recovery codes ----

const recoveryCodeCount = 10

// recoveryAlphabet has no letters that read like others (0/o, 1/l/i).
const recoveryAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newRecoveryCodes makes a set of codes such as "k7mqx-3vh9p", and their hashes.
func newRecoveryCodes() (codes []string, hashes [][]byte) {
	for range recoveryCodeCount {
		b := make([]byte, 10)
		rand.Read(b)
		var c strings.Builder
		for i, x := range b {
			if i == 5 {
				c.WriteByte('-')
			}
			c.WriteByte(recoveryAlphabet[int(x)%len(recoveryAlphabet)])
		}
		codes = append(codes, c.String())
		hashes = append(hashes, hashRecoveryCode(c.String()))
	}
	return codes, hashes
}

// hashRecoveryCode hashes a code as typed: case, spaces and dashes do not matter.
func hashRecoveryCode(code string) []byte {
	clean := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(code)))
	h := sha256.Sum256([]byte("ekpanel-recovery:" + clean))
	return h[:]
}

// looksLikeRecoveryCode: the sign-in's code field holds a recovery code, not six digits.
func looksLikeRecoveryCode(code string) bool {
	clean := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(code))
	return len(clean) == 10
}

type userWithCodes struct {
	store.User
	RecoveryCodes []string `json:"recovery_codes"`
}

func (s *Server) handleRecoveryCodesLeft(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	n, err := s.store.RecoveryCodesLeft(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"left": n})
}

// handleNewRecoveryCodes replaces the codes; the password is asked again.
func (s *Server) handleNewRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	if !u.TOTPEnabled {
		writeError(w, http.StatusConflict, "totp_not_enabled", "turn on two-factor authentication first")
		return
	}
	if !s.checkPasswordAgain(w, u, req.Password) {
		return
	}
	codes, hashes := newRecoveryCodes()
	if err := s.store.SetRecoveryCodes(r.Context(), u.ID, hashes); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.recovery_codes_new", u.Email, nil)
	writeJSON(w, http.StatusOK, map[string][]string{"recovery_codes": codes})
}

// checkPasswordAgain checks the account's password for a sensitive change, with the
// re-authentication rate limit. It writes the error answer.
func (s *Server) checkPasswordAgain(w http.ResponseWriter, u *store.User, password string) bool {
	key := "reauth:" + u.ID
	if s.limiter.Blocked(key) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; try again later")
		return false
	}
	if ok, err := auth.VerifyPassword(password, u.PasswordHash); err != nil || !ok {
		s.limiter.Fail(key)
		writeError(w, http.StatusForbidden, "invalid_password", "the password is not correct")
		return false
	}
	return true
}

// handleResetCustomerTOTP turns a customer's two-factor off (they lost their phone and
// codes); they sign in with the password and can turn it on again.
func (s *Server) handleResetCustomerTOTP(w http.ResponseWriter, r *http.Request) {
	c, err := s.getCustomer(r, urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	if s.resetTOTP(w, r, c.ID) {
		s.audit(r, "customer.2fa_reset", c.Email, map[string]any{"customer_id": c.ID})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) resetTOTP(w http.ResponseWriter, r *http.Request, id string) bool {
	if err := s.store.SetTOTP(r.Context(), id, nil, false); err != nil {
		s.internalError(w, r, err)
		return false
	}
	if err := s.store.SetRecoveryCodes(r.Context(), id, nil); err != nil {
		s.internalError(w, r, err)
		return false
	}
	return true
}

// ---- Sessions ----

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	list, err := s.store.ListSessions(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	for i := range list {
		list[i].Current = bytes.Equal(list[i].TokenHash, hash)
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	id := chi.URLParam(r, "id")
	list, err := s.store.ListSessions(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	for _, x := range list {
		if x.ID == id && bytes.Equal(x.TokenHash, hash) {
			writeError(w, http.StatusBadRequest, "session_current", "sign out to end this session")
			return
		}
	}
	ok, err := s.store.DeleteUserSession(r.Context(), u.ID, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "session_not_found", "no such session")
		return
	}
	s.audit(r, "auth.session_ended", u.Email, map[string]any{"session_id": id})
	w.WriteHeader(http.StatusNoContent)
}
