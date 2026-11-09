package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// ---- Packages ----

func (s *Server) validPackage(w http.ResponseWriter, r *http.Request, l *store.PackageLimits) bool {
	fail := func(code, msg string) bool {
		writeError(w, http.StatusBadRequest, code, msg)
		return false
	}
	l.Name = strings.TrimSpace(l.Name)
	switch {
	case l.Name == "" || utf8.RuneCountInString(l.Name) > 60:
		return fail("invalid_name", "enter a name of up to 60 characters")
	case l.MaxSites < 0 || l.MaxDatabases < 0 || l.MaxApps < 0 || l.MaxCronJobs < 0 || l.MaxSites > 100000 || l.MaxDatabases > 100000 ||
		l.MaxApps > 100000 || l.MaxCronJobs > 100000 || l.MaxMailDomains < 0 || l.MaxMailDomains > 100000 ||
		l.MaxMailboxes < 0 || l.MaxMailboxes > 100000:
		return fail("invalid_package", "counts must be between 0 and 100,000")
	case l.DiskMB != nil && (*l.DiskMB < 1 || *l.DiskMB > 1<<30):
		return fail("invalid_package", "the disk space must be at least 1 MB")
	case l.MemoryMB != nil && (*l.MemoryMB < 64 || *l.MemoryMB > 1<<30):
		return fail("invalid_package", "the memory limit must be at least 64 MB")
	case l.CPUPercent != nil && (*l.CPUPercent < 5 || *l.CPUPercent > 102400):
		return fail("invalid_package", "the CPU limit must be at least 5%")
	case l.MailboxQuotaMB != nil && (*l.MailboxQuotaMB < 1 || *l.MailboxQuotaMB > agentproto.MaxMailboxQuotaMB):
		return fail("invalid_package", "the mailbox size must be at least 1 MB")
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
		img = strings.TrimSpace(img)
		if img == "" {
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

func (s *Server) writePackageError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrPackageNameTaken):
		writeError(w, http.StatusConflict, "package_name_taken", err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "package_not_found", "no package with that ID")
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "package_in_use", "move its customers to another package first")
	default:
		s.internalError(w, r, err)
	}
}

func urlID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListPackages(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreatePackage(w http.ResponseWriter, r *http.Request) {
	var l store.PackageLimits
	if !readJSON(w, r, &l) || !s.validPackage(w, r, &l) {
		return
	}
	p, err := s.store.CreatePackage(r.Context(), l)
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	s.audit(r, "package.created", p.Name, map[string]any{"package_id": p.ID})
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleGetPackage(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPackage(r.Context(), urlID(r))
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleUpdatePackage changes a package. New limits apply to its customers' servers at once;
// resources they already have over a lowered count limit stay.
func (s *Server) handleUpdatePackage(w http.ResponseWriter, r *http.Request) {
	var l store.PackageLimits
	if !readJSON(w, r, &l) || !s.validPackage(w, r, &l) {
		return
	}
	p, err := s.store.UpdatePackage(r.Context(), urlID(r), l)
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	s.audit(r, "package.updated", p.Name, map[string]any{"package_id": p.ID})
	s.reapplyCustomersOf(r, p.ID)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPackage(r.Context(), urlID(r))
	if err == nil {
		err = s.store.DeletePackage(r.Context(), p.ID)
	}
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	s.audit(r, "package.deleted", p.Name, map[string]any{"package_id": p.ID})
	w.WriteHeader(http.StatusNoContent)
}

// reapplyCustomersOf queues a reconcile of every server where the package's customers have
// something, so changed limits reach those servers.
func (s *Server) reapplyCustomersOf(r *http.Request, packageID string) {
	ids, err := s.store.ServersOfPackage(r.Context(), packageID)
	if err != nil {
		s.log.Error("servers of package", "package", packageID, "err", err)
		return
	}
	for _, id := range ids {
		s.queueReconcile(r, id)
	}
}

// ---- Customers ----

type customerRequest struct {
	Email     string `json:"email"`
	Name      string `json:"name"`
	PackageID string `json:"package_id"`
	Locale    string `json:"locale"`
}

// customerWithPassword carries a new password; it is shown only this once.
type customerWithPassword struct {
	Customer store.Customer `json:"customer"`
	Password string         `json:"password"`
}

func (s *Server) writeCustomerError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrDuplicateEmail):
		writeError(w, http.StatusConflict, "email_taken", "another account uses this email address")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "customer_not_found", "no customer with that ID")
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "customer_has_resources", "delete or move the customer's websites, databases and apps first")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) validCustomer(w http.ResponseWriter, r *http.Request, req *customerRequest) bool {
	email, name, ok := validateAccount(w, req.Email, req.Name, strings.Repeat("x", auth.MinPasswordLength))
	if !ok {
		return false
	}
	req.Email, req.Name = email, name
	if !isUUID(req.PackageID) {
		writeError(w, http.StatusBadRequest, "package_not_found", "choose a package")
		return false
	}
	if _, err := s.store.GetPackage(r.Context(), req.PackageID); err != nil {
		writeError(w, http.StatusBadRequest, "package_not_found", "choose a package")
		return false
	}
	return true
}

func (s *Server) handleListCustomers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListCustomers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateCustomer makes the account with a generated password, returned only once.
func (s *Server) handleCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerRequest
	if !readJSON(w, r, &req) || !s.validCustomer(w, r, &req) {
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
	c, err := s.store.CreateCustomer(r.Context(), store.NewCustomer{
		Email: req.Email, Name: req.Name, PasswordHash: hash, Locale: locale, PackageID: req.PackageID,
	})
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.created", c.Email, map[string]any{"customer_id": c.ID, "package_id": req.PackageID})
	writeJSON(w, http.StatusCreated, customerWithPassword{Customer: c, Password: password})
}

func (s *Server) handleGetCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleUpdateCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerRequest
	if !readJSON(w, r, &req) || !s.validCustomer(w, r, &req) {
		return
	}
	before, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	c, err := s.store.UpdateCustomer(r.Context(), before.ID, req.Email, req.Name, req.PackageID)
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.updated", c.Email, map[string]any{"customer_id": c.ID, "package_id": req.PackageID})
	if *before.PackageID != req.PackageID {
		s.reapplyOwner(r, c.ID)
	}
	writeJSON(w, http.StatusOK, c)
}

// reapplyOwner queues a reconcile of every server where the customer has something.
func (s *Server) reapplyOwner(r *http.Request, ownerID string) {
	ids, err := s.store.ServersOfOwner(r.Context(), ownerID)
	if err != nil {
		s.log.Error("servers of customer", "customer", ownerID, "err", err)
		return
	}
	for _, id := range ids {
		s.queueReconcile(r, id)
	}
}

func (s *Server) handleResetCustomerPassword(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	password := auth.NewDatabasePassword()
	hash, err := auth.HashPassword(password)
	if err == nil {
		err = s.store.SetPassword(r.Context(), c.ID, hash)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "customer.password_reset", c.Email, map[string]any{"customer_id": c.ID})
	writeJSON(w, http.StatusOK, customerWithPassword{Customer: c, Password: password})
}

func (s *Server) handleDeleteCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err == nil {
		err = s.store.DeleteCustomer(r.Context(), c.ID)
	}
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.deleted", c.Email, map[string]any{"customer_id": c.ID})
	w.WriteHeader(http.StatusNoContent)
}

// ---- Signing in as a customer ----

// returnCookieName holds the administrator's own session while they act as a customer, so
// leaving goes back to it instead of creating a new administrator session.
func (s *Server) returnCookieName() string { return s.cookieName() + "_return" }

func (s *Server) setReturnCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: s.returnCookieName(), Value: token, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.opts.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) handleImpersonate(w http.ResponseWriter, r *http.Request) {
	admin := currentUser(r)
	if admin.ImpersonatorID != nil {
		writeError(w, http.StatusConflict, "already_impersonating", "go back to your own account first")
		return
	}
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	own, err := r.Cookie(s.cookieName())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
		return
	}
	token := auth.NewToken()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateImpersonationSession(r.Context(), auth.HashToken(token), c.ID, admin.ID, clientIP(r), ua); err != nil {
		s.internalError(w, r, err)
		return
	}
	ttl := int(store.ImpersonationTTL.Seconds())
	s.setReturnCookie(w, own.Value, ttl)
	s.setSessionCookie(w, token, ttl)
	s.audit(r, "customer.impersonated", c.Email, map[string]any{"customer_id": c.ID})
	writeJSON(w, http.StatusOK, c.User)
}

// handleEndImpersonation signs out of the customer's account and back into the
// administrator's own session, if it is still valid.
func (s *Server) handleEndImpersonation(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.ImpersonatorID == nil {
		writeError(w, http.StatusConflict, "not_impersonating", "you are signed in as yourself")
		return
	}
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	if err := s.store.DeleteSession(r.Context(), hash); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.auditAs(r, u.ImpersonatorID, "customer.impersonation_ended", u.Email, map[string]any{"customer_id": u.ID})
	ret, err := r.Cookie(s.returnCookieName())
	s.setReturnCookie(w, "", -1)
	if err != nil || ret.Value == "" {
		s.setSessionCookie(w, "", -1)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in again")
		return
	}
	admin, err := s.store.SessionUser(r.Context(), auth.HashToken(ret.Value))
	if err != nil || admin.ID != *u.ImpersonatorID {
		s.setSessionCookie(w, "", -1)
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in again")
		return
	}
	s.setSessionCookie(w, ret.Value, int(store.SessionTTL.Seconds()))
	writeJSON(w, http.StatusOK, admin)
}

// ---- The signed-in user's own account ----

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	if u.ImpersonatorID != nil {
		writeError(w, http.StatusForbidden, "forbidden", "an administrator cannot change a customer's password this way")
		return
	}
	if ok, err := auth.VerifyPassword(req.Current, u.PasswordHash); err != nil || !ok {
		writeError(w, http.StatusBadRequest, "invalid_password", "the current password is not correct")
		return
	}
	if _, _, ok := validateAccount(w, u.Email, u.Name, req.New); !ok {
		return
	}
	hash, err := auth.HashPassword(req.New)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Signs out every session, this one too; start a fresh one.
	if err := s.store.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.startSession(w, r, u); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.password_changed", u.Email, nil)
	w.WriteHeader(http.StatusNoContent)
}

type myUsage struct {
	Package *store.Package `json:"package"`
	Usage   store.Usage    `json:"usage"`
	// Bytes served by the customer's websites and apps this month (UTC).
	BandwidthMonth int64 `json:"bandwidth_month"`
}

// handleMyUsage shows a customer their package and what they use of it.
func (s *Server) handleMyUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role != "customer" {
		writeJSON(w, http.StatusOK, myUsage{})
		return
	}
	c, err := s.store.GetCustomer(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	pkg.Customers = 0 // not the customer's business
	now := time.Now().UTC()
	bw, err := s.store.OwnerBandwidth(r.Context(), u.ID, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, myUsage{Package: &pkg, Usage: c.Usage, BandwidthMonth: bw})
}

// customerServerView is what a customer may know about a server: enough to pick one.
type customerServerView struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Online bool   `json:"online"`
	// The UI checks enrolled_at to offer only connected servers.
	EnrolledAt any `json:"enrolled_at"`
}

func (s *Server) customerServers(w http.ResponseWriter, r *http.Request) {
	pkg, err := s.packageFor(r, &currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	list, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := []customerServerView{}
	for _, srv := range list {
		if srv.EnrolledAt == nil || (len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, srv.ID)) {
			continue
		}
		online, _ := s.hub.Latest(srv.ID)
		out = append(out, customerServerView{ID: srv.ID, Name: srv.Name, Online: online, EnrolledAt: srv.EnrolledAt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleMeasureUsage measures disk use on a server now instead of at the next half hour.
func (s *Server) handleMeasureUsage(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	if err := s.opts.Tasks.MeasureDiskUse(r.Context(), id); err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
