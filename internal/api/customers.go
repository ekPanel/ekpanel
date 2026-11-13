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
	case l.BandwidthGB != nil && (*l.BandwidthGB < 1 || *l.BandwidthGB > 1<<20):
		return fail("invalid_package", "the bandwidth must be at least 1 GB")
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

// getPackage returns a package the signed-in user may manage: a reseller only their own.
func (s *Server) getPackage(r *http.Request, id string) (store.Package, error) {
	p, err := s.store.GetPackage(r.Context(), id)
	if err == nil && isReseller(r) && (p.ResellerID == nil || *p.ResellerID != currentUser(r).ID) {
		return store.Package{}, store.ErrNotFound
	}
	return p, err
}

// packageFitsReseller checks a reseller's package against what the reseller was given: no
// count above theirs, no unlimited or larger size where theirs is limited, and only the PHP
// versions, images, servers and features they have. Images matter only for a package with
// apps, and the mailbox size only for one with mailboxes.
func (s *Server) packageFitsReseller(w http.ResponseWriter, r *http.Request, l *store.PackageLimits, resellerID *string) bool {
	if resellerID == nil {
		return true
	}
	rl, err := s.store.GetResellerLimits(r.Context(), *resellerID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	within := func(limit, v *int) bool { return limit == nil || (v != nil && *v <= *limit) }
	subset := func(limit, v []string) bool {
		if len(limit) == 0 {
			return true
		}
		return len(v) > 0 && !slices.ContainsFunc(v, func(x string) bool { return !slices.Contains(limit, x) })
	}
	field := ""
	switch {
	case l.MaxSites > rl.MaxSites:
		field = "max_sites"
	case l.MaxDatabases > rl.MaxDatabases:
		field = "max_databases"
	case l.MaxApps > rl.MaxApps:
		field = "max_apps"
	case l.MaxCronJobs > rl.MaxCronJobs:
		field = "max_cron_jobs"
	case l.MaxMailDomains > rl.MaxMailDomains:
		field = "max_mail_domains"
	case l.MaxMailboxes > rl.MaxMailboxes:
		field = "max_mailboxes"
	case !within(rl.DiskMB, l.DiskMB):
		field = "disk_mb"
	case !within(rl.BandwidthGB, l.BandwidthGB):
		field = "bandwidth_gb"
	case !within(rl.MemoryMB, l.MemoryMB):
		field = "memory_mb"
	case !within(rl.CPUPercent, l.CPUPercent):
		field = "cpu_percent"
	case l.MaxMailboxes > 0 && !within(rl.MailboxQuotaMB, l.MailboxQuotaMB):
		field = "mailbox_quota_mb"
	case !subset(rl.PHPVersions, l.PHPVersions):
		field = "php_versions"
	case !subset(rl.ServerIDs, l.ServerIDs):
		field = "server_ids"
	case l.MaxApps > 0 && len(rl.AppImages) > 0 && (len(l.AppImages) == 0 ||
		slices.ContainsFunc(l.AppImages, func(img string) bool { return !imagesAllow(rl.AppImages, img) })):
		field = "app_images"
	case l.SFTP && !rl.SFTP:
		field = "sftp"
	case l.FTP && !rl.FTP:
		field = "ftp"
	case l.GitApps && !rl.GitApps:
		field = "git_apps"
	}
	if field != "" {
		writeErrorData(w, http.StatusBadRequest, "package_over_reseller", "the package gives more than the reseller's account allows",
			map[string]any{"field": field})
		return false
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

// handleListPackages lists every package for administrators, and a reseller's own for them.
func (s *Server) handleListPackages(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListPackages(r.Context(), ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreatePackage(w http.ResponseWriter, r *http.Request) {
	var l store.PackageLimits
	var reseller *string
	if isReseller(r) {
		reseller = &currentUser(r).ID
	}
	if !readJSON(w, r, &l) || !s.validPackage(w, r, &l) || !s.packageFitsReseller(w, r, &l, reseller) {
		return
	}
	p, err := s.store.CreatePackage(r.Context(), l, reseller)
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	s.audit(r, "package.created", p.Name, map[string]any{"package_id": p.ID})
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleGetPackage(w http.ResponseWriter, r *http.Request) {
	p, err := s.getPackage(r, urlID(r))
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
	if !readJSON(w, r, &l) {
		return
	}
	before, err := s.getPackage(r, urlID(r))
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	if !s.validPackage(w, r, &l) || !s.packageFitsReseller(w, r, &l, before.ResellerID) {
		return
	}
	p, err := s.store.UpdatePackage(r.Context(), before.ID, l)
	if err != nil {
		s.writePackageError(w, r, err)
		return
	}
	s.audit(r, "package.updated", p.Name, map[string]any{"package_id": p.ID})
	s.reapplyCustomersOf(r, p.ID)
	s.opts.Tasks.CheckBandwidth(r.Context()) // a new limit may block or unblock websites
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeletePackage(w http.ResponseWriter, r *http.Request) {
	p, err := s.getPackage(r, urlID(r))
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
	// For administrators: the reseller whose customer this is ("" for the administrators';
	// left out, it stays as it is). Resellers' customers are always their own.
	ResellerID *string `json:"reseller_id"`
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

// getCustomer returns a customer the signed-in user may manage: a reseller only their own.
func (s *Server) getCustomer(r *http.Request, id string) (store.Customer, error) {
	c, err := s.store.GetCustomer(r.Context(), id)
	if err == nil && isReseller(r) && (c.ResellerID == nil || *c.ResellerID != currentUser(r).ID) {
		return store.Customer{}, store.ErrNotFound
	}
	return c, err
}

// customerReseller decides whose customer a new or changed customer is: a reseller's own;
// for administrators, the reseller named in the request (current when left out).
func (s *Server) customerReseller(w http.ResponseWriter, r *http.Request, req *customerRequest, current *string) (*string, bool) {
	switch {
	case isReseller(r):
		return &currentUser(r).ID, true
	case req.ResellerID == nil:
		return current, true
	case *req.ResellerID == "":
		return nil, true
	}
	u, err := s.store.UserByID(r.Context(), urlSafeID(*req.ResellerID))
	if err != nil || u.Role != "reseller" {
		writeError(w, http.StatusBadRequest, "reseller_not_found", "choose a reseller")
		return nil, false
	}
	return &u.ID, true
}

// validCustomer checks the account and that the package belongs to the customer's reseller
// (or to the administrators, for theirs).
func (s *Server) validCustomer(w http.ResponseWriter, r *http.Request, req *customerRequest, reseller *string) bool {
	email, name, ok := validateAccount(w, req.Email, req.Name, strings.Repeat("x", auth.MinPasswordLength))
	if !ok {
		return false
	}
	req.Email, req.Name = email, name
	if !isUUID(req.PackageID) {
		writeError(w, http.StatusBadRequest, "package_not_found", "choose a package")
		return false
	}
	p, err := s.store.GetPackage(r.Context(), req.PackageID)
	if err != nil || (p.ResellerID == nil) != (reseller == nil) || (reseller != nil && *p.ResellerID != *reseller) {
		writeError(w, http.StatusBadRequest, "package_not_found", "choose a package")
		return false
	}
	return true
}

// resellerHasRoom checks that a reseller may have one more customer.
func (s *Server) resellerHasRoom(w http.ResponseWriter, r *http.Request, reseller string) bool {
	l, err := s.store.GetResellerLimits(r.Context(), reseller)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	list, err := s.store.CustomersOfReseller(r.Context(), reseller)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	if len(list) >= l.MaxCustomers {
		writeError(w, http.StatusConflict, "reseller_limit_customer", "the reseller's account allows no more customers")
		return false
	}
	return true
}

// handleListCustomers lists every customer for administrators (or one reseller's, with
// ?reseller_id=), and a reseller's own for them.
func (s *Server) handleListCustomers(w http.ResponseWriter, r *http.Request) {
	filter := ownerScope(r)
	if id := r.URL.Query().Get("reseller_id"); isAdmin(r) && id != "" {
		filter = urlSafeID(id)
	}
	list, err := s.store.ListCustomers(r.Context(), filter)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateCustomer makes the account with a generated password, returned only once.
func (s *Server) handleCreateCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerRequest
	if !readJSON(w, r, &req) {
		return
	}
	reseller, ok := s.customerReseller(w, r, &req, nil)
	if !ok || !s.validCustomer(w, r, &req, reseller) || (reseller != nil && !s.resellerHasRoom(w, r, *reseller)) {
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
		Email: req.Email, Name: req.Name, PasswordHash: hash, Locale: locale, PackageID: req.PackageID, ResellerID: reseller,
	})
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.created", c.Email, map[string]any{"customer_id": c.ID, "package_id": req.PackageID, "reseller_id": reseller})
	writeJSON(w, http.StatusCreated, customerWithPassword{Customer: c, Password: password})
}

func (s *Server) handleGetCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.getCustomer(r, urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleUpdateCustomer(w http.ResponseWriter, r *http.Request) {
	var req customerRequest
	if !readJSON(w, r, &req) {
		return
	}
	before, err := s.getCustomer(r, urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	reseller, ok := s.customerReseller(w, r, &req, before.ResellerID)
	if !ok || !s.validCustomer(w, r, &req, reseller) {
		return
	}
	moved := !sameID(reseller, before.ResellerID)
	if moved && reseller != nil {
		// Everything the customer has now counts for the new reseller.
		servers, err := s.store.ServersOfOwner(r.Context(), before.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !s.resellerHasRoom(w, r, *reseller) || !s.resellerAllowsFor(w, r, *reseller, before.Usage, servers...) {
			return
		}
	}
	c, err := s.store.UpdateCustomer(r.Context(), before.ID, req.Email, req.Name, req.PackageID, reseller)
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.updated", c.Email, map[string]any{"customer_id": c.ID, "package_id": req.PackageID, "reseller_id": reseller})
	if *before.PackageID != req.PackageID || moved {
		s.reapplyOwner(r, c.ID)
		check := []string{c.ID}
		if before.ResellerID != nil {
			check = append(check, *before.ResellerID)
		}
		s.opts.Tasks.CheckBandwidth(r.Context(), check...)
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
	c, err := s.getCustomer(r, urlID(r))
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
	c, err := s.getCustomer(r, urlID(r))
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
	if currentUser(r).ImpersonatorID != nil {
		writeError(w, http.StatusConflict, "already_impersonating", "go back to your own account first")
		return
	}
	c, err := s.getCustomer(r, urlID(r))
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	if s.impersonate(w, r, c.User) {
		s.audit(r, "customer.impersonated", c.Email, map[string]any{"customer_id": c.ID})
		writeJSON(w, http.StatusOK, c.User)
	}
}

// impersonate starts a session as someone else (an administrator as a customer or a
// reseller, a reseller as their customer), keeping the caller's own session for going back.
func (s *Server) impersonate(w http.ResponseWriter, r *http.Request, c store.User) bool {
	admin := currentUser(r)
	if admin.ImpersonatorID != nil {
		writeError(w, http.StatusConflict, "already_impersonating", "go back to your own account first")
		return false
	}
	// Only from the administrator's own browser session (never with an API token): the
	// cookie kept for going back must be that session.
	if _, ok := r.Context().Value(sessionHashKey).([]byte); !ok || currentToken(r) != nil {
		writeError(w, http.StatusForbidden, "token_not_allowed", "this needs a signed-in session; API tokens cannot do it")
		return false
	}
	own, err := r.Cookie(s.cookieName())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in first")
		return false
	}
	token := auth.NewToken()
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.CreateImpersonationSession(r.Context(), auth.HashToken(token), c.ID, admin.ID, clientIP(r), ua); err != nil {
		s.internalError(w, r, err)
		return false
	}
	ttl := int(store.ImpersonationTTL.Seconds())
	s.setReturnCookie(w, own.Value, ttl)
	s.setSessionCookie(w, token, ttl)
	return true
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
	// For a reseller: their limits and what all their customers have.
	Reseller *store.Reseller `json:"reseller,omitempty"`
	Usage    store.Usage     `json:"usage"`
	// Bytes served by the customer's websites and apps this month (UTC).
	BandwidthMonth int64 `json:"bandwidth_month"`
}

// handleMyUsage shows a customer their package and what they use of it, and a reseller
// their limits and what their customers use.
func (s *Server) handleMyUsage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Role == "reseller" {
		rs, err := s.store.GetReseller(r.Context(), u.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, myUsage{Reseller: &rs, Usage: rs.Usage, BandwidthMonth: rs.BandwidthMonth})
		return
	}
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
	of, err := s.offerFor(r)
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
		if srv.EnrolledAt == nil || (len(of.ServerIDs) > 0 && !slices.Contains(of.ServerIDs, srv.ID)) {
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

// sameID reports whether two optional IDs are the same (both nil counts).
func sameID(a, b *string) bool { return (a == nil && b == nil) || (a != nil && b != nil && *a == *b) }
