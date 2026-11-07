package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"ekpanel/internal/store"
)

// Who may see what. Administrators see everything; a customer sees only what they own.
// Everything a customer may not see is reported as not found, so IDs cannot be probed.

func isAdmin(r *http.Request) bool { return currentUser(r).Role == "admin" }

func visible(r *http.Request, ownerID *string) bool {
	u := currentUser(r)
	return u.Role == "admin" || (ownerID != nil && *ownerID == u.ID)
}

// ownerScope is the owner to filter lists by: empty for administrators.
func ownerScope(r *http.Request) string {
	if isAdmin(r) {
		return ""
	}
	return currentUser(r).ID
}

func (s *Server) getSite(r *http.Request, id string) (store.Site, error) {
	site, err := s.store.GetSite(r.Context(), id)
	if err == nil && !visible(r, site.OwnerID) {
		return store.Site{}, store.ErrNotFound
	}
	return site, err
}

func (s *Server) getDatabase(r *http.Request, id string) (store.Database, error) {
	d, err := s.store.GetDatabase(r.Context(), id)
	if err == nil && !visible(r, d.OwnerID) {
		return store.Database{}, store.ErrNotFound
	}
	return d, err
}

func (s *Server) getApp(r *http.Request, id string) (store.App, error) {
	a, err := s.store.GetApp(r.Context(), id)
	if err == nil && !visible(r, a.OwnerID) {
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
// an administrator may create it for a customer by naming them in owner_id.
func (s *Server) resolveOwner(w http.ResponseWriter, r *http.Request, requested string) (placement, bool) {
	u := currentUser(r)
	ownerID := ""
	switch {
	case u.Role == "customer":
		ownerID = u.ID
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
	if errors.Is(err, store.ErrNotFound) {
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
	return true
}

func overQuota(pkg *store.Package, u store.Usage) bool {
	return pkg.DiskMB != nil && u.DiskBytes >= int64(*pkg.DiskMB)<<20
}

// ownerOverQuota reports whether the owner of a resource has used up their disk space.
func (s *Server) ownerOverQuota(r *http.Request, ownerID *string) (bool, error) {
	if ownerID == nil {
		return false, nil
	}
	c, err := s.store.GetCustomer(r.Context(), *ownerID)
	if err != nil {
		return false, err
	}
	pkg, err := s.store.GetPackage(r.Context(), *c.PackageID)
	if err != nil {
		return false, err
	}
	return overQuota(&pkg, c.Usage), nil
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
func imageAllowed(pkg *store.Package, image string) bool {
	if len(pkg.AppImages) == 0 {
		return true
	}
	for _, p := range pkg.AppImages {
		if image == p || strings.HasPrefix(image, p+":") || strings.HasPrefix(image, p+"@") ||
			(strings.HasSuffix(p, "/") && strings.HasPrefix(image, p)) {
			return true
		}
	}
	return false
}
