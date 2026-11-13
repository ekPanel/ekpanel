package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"ekpanel/internal/store"
)

// Who may see what. Administrators see everything; a customer sees only what they own; a
// reseller sees what their customers own (and owns nothing themselves). Everything one may
// not see is reported as not found, so IDs cannot be probed.

func isAdmin(r *http.Request) bool    { return currentUser(r).Role == "admin" }
func isReseller(r *http.Request) bool { return currentUser(r).Role == "reseller" }

func (s *Server) visible(r *http.Request, ownerID *string) bool {
	u := currentUser(r)
	switch {
	case u.Role == "admin":
		return true
	case ownerID == nil:
		return false
	case u.Role == "reseller":
		return s.isMyCustomer(r, *ownerID)
	}
	return *ownerID == u.ID
}

// isMyCustomer reports whether id is a customer of the signed-in reseller.
func (s *Server) isMyCustomer(r *http.Request, id string) bool {
	reseller, err := s.store.ResellerOf(r.Context(), id)
	return err == nil && reseller != nil && *reseller == currentUser(r).ID
}

// ownerScope is the owner to filter lists by: empty for administrators. The store reads a
// reseller's ID as "owned by one of their customers".
func ownerScope(r *http.Request) string {
	if isAdmin(r) {
		return ""
	}
	return currentUser(r).ID
}

func (s *Server) getSite(r *http.Request, id string) (store.Site, error) {
	site, err := s.store.GetSite(r.Context(), id)
	if err == nil && !s.visible(r, site.OwnerID) {
		return store.Site{}, store.ErrNotFound
	}
	return site, err
}

func (s *Server) getDatabase(r *http.Request, id string) (store.Database, error) {
	d, err := s.store.GetDatabase(r.Context(), id)
	if err == nil && !s.visible(r, d.OwnerID) {
		return store.Database{}, store.ErrNotFound
	}
	return d, err
}

func (s *Server) getApp(r *http.Request, id string) (store.App, error) {
	a, err := s.store.GetApp(r.Context(), id)
	if err == nil && !s.visible(r, a.OwnerID) {
		return store.App{}, store.ErrNotFound
	}
	return a, err
}

// placement is the owner of a new resource and the package that limits it (nil for
// resources of the administrators).
type placement struct {
	OwnerID *string
	Package *store.Package
}

// resolveOwner decides who will own a new resource: a customer always owns what they create;
// an administrator may create it for a customer by naming them in owner_id; a reseller must
// name one of their customers.
func (s *Server) resolveOwner(w http.ResponseWriter, r *http.Request, requested string) (placement, bool) {
	u := currentUser(r)
	ownerID := ""
	switch {
	case u.Role == "customer":
		ownerID = u.ID
	case u.Role == "reseller" && strings.TrimSpace(requested) == "":
		writeError(w, http.StatusBadRequest, "customer_required", "choose the customer this is for")
		return placement{}, false
	case strings.TrimSpace(requested) != "":
		ownerID = strings.TrimSpace(requested)
		if !isUUID(ownerID) {
			writeError(w, http.StatusBadRequest, "customer_not_found", "choose a customer")
			return placement{}, false
		}
	default:
		return placement{}, true
	}
	c, err := s.store.GetCustomer(r.Context(), ownerID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && isReseller(r) && (c.ResellerID == nil || *c.ResellerID != u.ID)) {
		writeError(w, http.StatusBadRequest, "customer_not_found", "choose a customer")
		return placement{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return placement{}, false
	}
	pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
	if err != nil {
		s.internalError(w, r, err)
		return placement{}, false
	}
	return placement{OwnerID: &c.ID, Package: &pkg}, true
}

// allowNew checks a package's limits for one more resource of a kind ("site", "database"
// or "app") on a server. It writes the error response and returns false when refused.
func (s *Server) allowNew(w http.ResponseWriter, r *http.Request, p placement, kind, serverID string) bool {
	if p.Package == nil {
		return true
	}
	pkg := p.Package
	if len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, serverID) {
		writeError(w, http.StatusBadRequest, "server_not_allowed", "this package cannot use that server")
		return false
	}
	usage, err := s.store.OwnerUsage(r.Context(), *p.OwnerID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	limit, have := 0, 0
	switch kind {
	case "site":
		limit, have = pkg.MaxSites, usage.Sites
	case "database":
		limit, have = pkg.MaxDatabases, usage.Databases
	case "app":
		limit, have = pkg.MaxApps, usage.Apps
	case "cron":
		limit, have = pkg.MaxCronJobs, usage.CronJobs
	case "mail_domain":
		limit, have = pkg.MaxMailDomains, usage.MailDomains
	}
	if have >= limit {
		writeError(w, http.StatusConflict, "limit_reached_"+kind, "the package allows no more of these")
		return false
	}
	if overQuota(pkg, usage) {
		writeError(w, http.StatusConflict, "quota_exceeded", "the disk space of the package is used up")
		return false
	}
	var add store.Usage
	switch kind {
	case "site":
		add.Sites = 1
	case "database":
		add.Databases = 1
	case "app":
		add.Apps = 1
	case "cron":
		add.CronJobs = 1
	case "mail_domain":
		add.MailDomains = 1
	}
	return s.resellerAllows(w, r, p.OwnerID, add, serverID)
}

// resellerAllows checks what the reseller of ownerID may still have: add comes on top, on
// the servers listed. Owners who are not a reseller's customer always fit. It writes the
// error response and returns false when refused.
func (s *Server) resellerAllows(w http.ResponseWriter, r *http.Request, ownerID *string, add store.Usage, servers ...string) bool {
	if ownerID == nil {
		return true
	}
	reseller, err := s.store.ResellerOf(r.Context(), *ownerID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	if reseller == nil {
		return true
	}
	return s.resellerAllowsFor(w, r, *reseller, add, servers...)
}

// resellerAllowsFor is resellerAllows for a reseller named directly.
func (s *Server) resellerAllowsFor(w http.ResponseWriter, r *http.Request, reseller string, add store.Usage, servers ...string) bool {
	ctx := r.Context()
	l, err := s.store.GetResellerLimits(ctx, reseller)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	for _, id := range servers {
		if id != "" && len(l.ServerIDs) > 0 && !slices.Contains(l.ServerIDs, id) {
			writeError(w, http.StatusBadRequest, "reseller_server", "the reseller's account cannot use that server")
			return false
		}
	}
	u, err := s.store.ResellerUsage(ctx, reseller)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	refuse := func(kind string) bool {
		writeError(w, http.StatusConflict, "reseller_limit_"+kind, "the reseller's account allows no more of these")
		return false
	}
	switch {
	case add.Sites > 0 && u.Sites+add.Sites > l.MaxSites:
		return refuse("site")
	case add.Databases > 0 && u.Databases+add.Databases > l.MaxDatabases:
		return refuse("database")
	case add.Apps > 0 && u.Apps+add.Apps > l.MaxApps:
		return refuse("app")
	case add.CronJobs > 0 && u.CronJobs+add.CronJobs > l.MaxCronJobs:
		return refuse("cron")
	case add.MailDomains > 0 && u.MailDomains+add.MailDomains > l.MaxMailDomains:
		return refuse("mail_domain")
	case add.Mailboxes > 0 && u.Mailboxes+add.Mailboxes > l.MaxMailboxes:
		return refuse("mailbox")
	}
	if l.DiskMB != nil {
		limit := int64(*l.DiskMB) << 20
		if u.DiskBytes+add.DiskBytes > limit || (add.DiskBytes == 0 && u.DiskBytes >= limit) {
			writeError(w, http.StatusConflict, "reseller_quota", "the disk space of the reseller's account is used up")
			return false
		}
	}
	return true
}

func overQuota(pkg *store.Package, u store.Usage) bool {
	return pkg.DiskMB != nil && u.DiskBytes >= int64(*pkg.DiskMB)<<20
}

// ownerOverQuota reports whether the owner of a resource has used up their disk space, or
// their reseller has.
func (s *Server) ownerOverQuota(r *http.Request, ownerID *string) (bool, error) {
	if ownerID == nil {
		return false, nil
	}
	c, err := s.store.GetCustomer(r.Context(), *ownerID)
	if err != nil {
		return false, err
	}
	pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
	if err != nil || overQuota(&pkg, c.Usage) || c.ResellerID == nil {
		return err == nil && overQuota(&pkg, c.Usage), err
	}
	l, err := s.store.GetResellerLimits(r.Context(), *c.ResellerID)
	if err != nil || l.DiskMB == nil {
		return false, err
	}
	u, err := s.store.ResellerUsage(r.Context(), *c.ResellerID)
	return err == nil && u.DiskBytes >= int64(*l.DiskMB)<<20, err
}

// offer is what the signed-in user may choose from when creating things: a customer's
// package, or a reseller's limits. Administrators get nil: everything.
type offer struct {
	PHPVersions, AppImages, ServerIDs []string
}

func (s *Server) offerFor(r *http.Request) (*offer, error) {
	u := currentUser(r)
	switch u.Role {
	case "customer":
		pkg, err := s.packageFor(r, &u.ID)
		if err != nil {
			return nil, err
		}
		return &offer{PHPVersions: pkg.PHPVersions, AppImages: pkg.AppImages, ServerIDs: pkg.ServerIDs}, nil
	case "reseller":
		l, err := s.store.GetResellerLimits(r.Context(), u.ID)
		if err != nil {
			return nil, err
		}
		return &offer{PHPVersions: l.PHPVersions, AppImages: l.AppImages, ServerIDs: l.ServerIDs}, nil
	}
	return nil, nil
}

// packageFor returns the package of a resource's owner, or nil for the administrators'.
func (s *Server) packageFor(r *http.Request, ownerID *string) (*store.Package, error) {
	if ownerID == nil {
		return nil, nil
	}
	c, err := s.store.GetCustomer(r.Context(), *ownerID)
	if err != nil {
		return nil, err
	}
	pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
	return &pkg, err
}

// imageAllowed matches an image against a package's allowed prefixes ("nginx", "ghcr.io/acme/").
func imageAllowed(pkg *store.Package, image string) bool { return imagesAllow(pkg.AppImages, image) }

// imagesAllow matches an image against allowed prefixes; none allows every image.
func imagesAllow(prefixes []string, image string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if image == p || strings.HasPrefix(image, p+":") || strings.HasPrefix(image, p+"@") ||
			(strings.HasSuffix(p, "/") && strings.HasPrefix(image, p)) {
			return true
		}
	}
	return false
}
