package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// Resellers are managed by administrators: their account, their limits, signing in as
// them, suspending them (which suspends their customers too).

type resellerRequest struct {
	Email  string               `json:"email"`
	Name   string               `json:"name"`
	Locale string               `json:"locale"`
	Limits store.ResellerLimits `json:"limits"`
}

type resellerWithPassword struct {
	Reseller store.Reseller `json:"reseller"`
	Password string         `json:"password"`
}

func (s *Server) writeResellerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrDuplicateEmail):
		writeError(w, http.StatusConflict, "email_taken", "another account uses this email address")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "reseller_not_found", "no reseller with that ID")
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "reseller_has_customers", "move or delete the reseller's customers first")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) validReseller(w http.ResponseWriter, r *http.Request, req *resellerRequest) bool {
	email, name, ok := validateAccount(w, req.Email, req.Name, strings.Repeat("x", auth.MinPasswordLength))
	if !ok {
		return false
	}
	req.Email, req.Name = email, name
	fail := func(code, msg string) bool {
		writeError(w, http.StatusBadRequest, code, msg)
		return false
	}
	l := &req.Limits
	counts := []int{l.MaxCustomers, l.MaxSites, l.MaxDatabases, l.MaxApps, l.MaxCronJobs, l.MaxMailDomains, l.MaxMailboxes}
	switch {
	case slices.ContainsFunc(counts, func(n int) bool { return n < 0 || n > 1000000 }):
		return fail("invalid_reseller_limits", "counts must be between 0 and 1,000,000")
	case l.DiskMB != nil && (*l.DiskMB < 1 || *l.DiskMB > 1<<30):
		return fail("invalid_reseller_limits", "the disk space must be at least 1 MB")
	case l.BandwidthGB != nil && (*l.BandwidthGB < 1 || *l.BandwidthGB > 1<<20):
		return fail("invalid_reseller_limits", "the bandwidth must be at least 1 GB")
	case l.MemoryMB != nil && (*l.MemoryMB < 64 || *l.MemoryMB > 1<<30):
		return fail("invalid_reseller_limits", "the memory limit must be at least 64 MB")
	case l.CPUPercent != nil && (*l.CPUPercent < 5 || *l.CPUPercent > 102400):
		return fail("invalid_reseller_limits", "the CPU limit must be at least 5%")
	case l.MailboxQuotaMB != nil && (*l.MailboxQuotaMB < 1 || *l.MailboxQuotaMB > agentproto.MaxMailboxQuotaMB):
		return fail("invalid_reseller_limits", "the mailbox size must be at least 1 MB")
	}
	if l.BandwidthGB == nil {
		l.BandwidthStop = false
	}
	if l.PHPVersions == nil {
		l.PHPVersions = []string{}
	}
	for _, v := range l.PHPVersions {
		if !slices.Contains(agentproto.PHPVersions, v) {
			return fail("invalid_php_version", "choose supported PHP versions")
		}
	}
	images := []string{}
	for _, img := range l.AppImages {
		if img = strings.TrimSpace(img); img == "" {
			continue
		}
		if !imageRefRE.MatchString(strings.TrimSuffix(img, "/")) || len(images) >= 100 {
			return fail("invalid_image", "allowed images are names such as nginx or ghcr.io/owner/")
		}
		images = append(images, img)
	}
	l.AppImages = images
	if l.ServerIDs == nil {
		l.ServerIDs = []string{}
	}
	for _, id := range l.ServerIDs {
		if !isUUID(id) {
			return fail("server_not_found", "choose existing servers")
		}
		if _, err := s.store.GetServer(r.Context(), id); err != nil {
			return fail("server_not_found", "choose existing servers")
		}
	}
	return true
}

func (s *Server) handleListResellers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListResellers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateReseller makes the account with a generated password, returned only once.
func (s *Server) handleCreateReseller(w http.ResponseWriter, r *http.Request) {
	var req resellerRequest
	if !readJSON(w, r, &req) || !s.validReseller(w, r, &req) {
		return
	}
	password := auth.NewDatabasePassword()
	hash, err := auth.HashPassword(password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	locale := normalizeLocale(req.Locale)
	if req.Locale == "" {
		locale = currentUser(r).Locale
	}
	rs, err := s.store.CreateReseller(r.Context(), store.NewReseller{Email: req.Email, Name: req.Name, PasswordHash: hash,
		Locale: locale, Limits: req.Limits})
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	s.audit(r, "reseller.created", rs.Email, map[string]any{"reseller_id": rs.ID})
	writeJSON(w, http.StatusCreated, resellerWithPassword{Reseller: rs, Password: password})
}

func (s *Server) handleGetReseller(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// handleUpdateReseller changes a reseller's account and limits. Lower limits stop new
// things; what the customers have already stays, and so do packages above the new limits.
func (s *Server) handleUpdateReseller(w http.ResponseWriter, r *http.Request) {
	var req resellerRequest
	if !readJSON(w, r, &req) || !s.validReseller(w, r, &req) {
		return
	}
	before, _ := s.store.GetResellerLimits(r.Context(), urlID(r))
	rs, err := s.store.UpdateReseller(r.Context(), urlID(r), req.Email, req.Name, req.Limits)
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	s.audit(r, "reseller.updated", rs.Email, map[string]any{"reseller_id": rs.ID})
	if before.WhiteLabel != rs.Limits.WhiteLabel {
		s.reapplyResellerDomain(r, rs.ID) // their panel domain starts or stops answering
	}
	s.opts.Tasks.CheckBandwidth(r.Context(), rs.ID) // a new limit may block or unblock websites
	s.writeReseller(w, r, rs.ID)
}

func (s *Server) handleDeleteReseller(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	var brand store.Brand
	if err == nil {
		brand, _ = s.store.GetBrand(r.Context(), &rs.ID)
		err = s.store.DeleteReseller(r.Context(), rs.ID)
	}
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	s.audit(r, "reseller.deleted", rs.Email, map[string]any{"reseller_id": rs.ID})
	if brand.Domain != nil {
		s.reapplyPanelServer(r)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetResellerPassword(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	password := auth.NewDatabasePassword()
	hash, err := auth.HashPassword(password)
	if err == nil {
		err = s.store.SetPassword(r.Context(), rs.ID, hash)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "reseller.password_reset", rs.Email, map[string]any{"reseller_id": rs.ID})
	writeJSON(w, http.StatusOK, resellerWithPassword{Reseller: rs, Password: password})
}

func (s *Server) handleResetResellerTOTP(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	if s.resetTOTP(w, r, rs.ID) {
		s.audit(r, "reseller.2fa_reset", rs.Email, map[string]any{"reseller_id": rs.ID})
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleImpersonateReseller(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	if s.impersonate(w, r, rs.User) {
		s.audit(r, "reseller.impersonated", rs.Email, map[string]any{"reseller_id": rs.ID})
		writeJSON(w, http.StatusOK, rs.User)
	}
}

// handleSuspendReseller suspends a reseller and, through them, all their customers.
func (s *Server) handleSuspendReseller(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if utf8.RuneCountInString(req.Reason) > 500 {
		writeError(w, http.StatusBadRequest, "invalid_reason", "the reason can be up to 500 characters")
		return
	}
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err == nil {
		err = s.store.SuspendUser(r.Context(), rs.ID, "reseller", req.Reason, currentUser(r).ID)
	}
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	s.audit(r, "reseller.suspended", rs.Email, map[string]any{"reseller_id": rs.ID, "reason": req.Reason})
	s.reapplyResellerCustomers(r, rs.ID)
	s.writeReseller(w, r, rs.ID)
}

func (s *Server) handleUnsuspendReseller(w http.ResponseWriter, r *http.Request) {
	rs, err := s.store.GetReseller(r.Context(), urlID(r))
	if err == nil {
		err = s.store.UnsuspendUser(r.Context(), rs.ID, "reseller")
	}
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	s.audit(r, "reseller.unsuspended", rs.Email, map[string]any{"reseller_id": rs.ID})
	s.reapplyResellerCustomers(r, rs.ID)
	s.writeReseller(w, r, rs.ID)
}

func (s *Server) writeReseller(w http.ResponseWriter, r *http.Request, id string) {
	rs, err := s.store.GetReseller(r.Context(), id)
	if err != nil {
		s.writeResellerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

// reapplyResellerCustomers queues a reconcile of every server where the reseller's
// customers have something.
func (s *Server) reapplyResellerCustomers(r *http.Request, id string) {
	list, err := s.store.CustomersOfReseller(r.Context(), id)
	if err != nil {
		s.log.Error("customers of reseller", "reseller", id, "err", err)
		return
	}
	servers := map[string]bool{}
	for _, c := range list {
		ids, err := s.store.ServersOfOwner(r.Context(), c)
		if err != nil {
			s.log.Error("servers of customer", "customer", c, "err", err)
			continue
		}
		for _, id := range ids {
			if !servers[id] {
				servers[id] = true
				s.queueReconcile(r, id)
			}
		}
	}
}

// reapplyResellerDomain has the panel's server pick up a reseller's panel domain, if any.
func (s *Server) reapplyResellerDomain(r *http.Request, id string) {
	if b, err := s.store.GetBrand(r.Context(), &id); err == nil && b.Domain != nil {
		s.reapplyPanelServer(r)
	}
}
