package api

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"ekpanel/internal/store"
)

// ---- Suspending ----

func (s *Server) handleSuspendCustomer(w http.ResponseWriter, r *http.Request) {
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
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err == nil {
		err = s.store.SuspendCustomer(r.Context(), c.ID, req.Reason)
	}
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.suspended", c.Email, map[string]any{"customer_id": c.ID, "reason": req.Reason})
	s.reapplyOwner(r, c.ID)
	s.writeCustomer(w, r, c.ID)
}

func (s *Server) handleUnsuspendCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCustomer(r.Context(), urlID(r))
	if err == nil {
		err = s.store.UnsuspendCustomer(r.Context(), c.ID)
	}
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	s.audit(r, "customer.unsuspended", c.Email, map[string]any{"customer_id": c.ID})
	s.reapplyOwner(r, c.ID)
	s.writeCustomer(w, r, c.ID)
}

func (s *Server) writeCustomer(w http.ResponseWriter, r *http.Request, id string) {
	c, err := s.store.GetCustomer(r.Context(), id)
	if err != nil {
		s.writeCustomerError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// ---- Changing the owner ----

type ownerChangeRequest struct {
	// To is a customer's ID; empty gives the things to the administrators.
	To          string   `json:"to"`
	Sites       []string `json:"sites"`
	Databases   []string `json:"databases"`
	Apps        []string `json:"apps"`
	MailDomains []string `json:"mail_domains"`
	Zones       []string `json:"zones"`
}

// ownerChangeTotals is what an owner change adds to the new owner.
type ownerChangeTotals struct {
	sites, databases, apps, cron, mailDomains, mailboxes int
	disk                                                 int64
	servers                                              []string
	names                                                []string // for the audit log
}

// uniqueIDs drops duplicates and refuses anything that is not an id.
func uniqueIDs(ids []string) ([]string, bool) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !isUUID(id) {
			return nil, false
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, true
}

// handleChangeOwner gives websites, databases, apps, email domains and DNS zones to another
// customer or to the administrators. Things the new owner already has are left as they are.
func (s *Server) handleChangeOwner(w http.ResponseWriter, r *http.Request) {
	var req ownerChangeRequest
	if !readJSON(w, r, &req) {
		return
	}
	lists := []*[]string{&req.Sites, &req.Databases, &req.Apps, &req.MailDomains, &req.Zones}
	total := 0
	for _, l := range lists {
		ids, ok := uniqueIDs(*l)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_params", "list things by their IDs")
			return
		}
		*l = ids
		total += len(ids)
	}
	if total == 0 || total > 1000 {
		writeError(w, http.StatusBadRequest, "nothing_to_give", "choose what to give to the new owner")
		return
	}
	ctx := r.Context()
	var to *string
	var target store.Customer
	if req.To != "" {
		c, err := s.store.GetCustomer(ctx, urlSafeID(req.To))
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "customer_not_found", "choose a customer")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		target, to = c, &c.ID
	}
	same := func(owner *string) bool {
		return (owner == nil && to == nil) || (owner != nil && to != nil && *owner == *to)
	}
	notFound := func(code string) {
		writeError(w, http.StatusNotFound, code, "one of the chosen things does not exist or is being deleted")
	}
	busy := func(name string) {
		writeErrorData(w, http.StatusConflict, "move_in_progress", name+" is being moved to another server; wait until that has finished",
			map[string]any{"name": name})
	}
	var add ownerChangeTotals
	givers := map[string]bool{} // customers giving websites or apps away: their bandwidth changes
	change := store.OwnerChange{To: to}
	addServer := func(id string) {
		if !slices.Contains(add.servers, id) {
			add.servers = append(add.servers, id)
		}
	}
	for _, id := range req.Sites {
		site, err := s.store.GetSite(ctx, id)
		if err != nil || site.State == "deleting" {
			notFound("site_not_found")
			return
		}
		if m, err := s.store.OpenMove(ctx, id); err == nil && copying(m) {
			busy(site.Domain)
			return
		}
		if same(site.OwnerID) {
			continue
		}
		jobs, err := s.store.ListCronJobs(ctx, store.CronFilter{SiteID: id})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if site.OwnerID != nil {
			givers[*site.OwnerID] = true
		}
		add.sites++
		add.cron += len(jobs)
		if site.DiskBytes != nil {
			add.disk += *site.DiskBytes
		}
		addServer(site.ServerID)
		add.names = append(add.names, site.Domain)
		change.Sites = append(change.Sites, id)
	}
	for _, id := range req.Databases {
		db, err := s.store.GetDatabase(ctx, id)
		if err != nil || db.State == "deleting" {
			notFound("database_not_found")
			return
		}
		if m, err := s.store.OpenDatabaseMove(ctx, id); err == nil && copying(m) {
			busy(db.Name)
			return
		}
		if same(db.OwnerID) {
			continue
		}
		add.databases++
		if db.SizeBytes != nil {
			add.disk += *db.SizeBytes
		}
		addServer(db.ServerID)
		add.names = append(add.names, db.Name)
		change.Databases = append(change.Databases, id)
	}
	for _, id := range req.Apps {
		app, err := s.store.GetApp(ctx, id)
		if err != nil || app.State == "deleting" {
			notFound("app_not_found")
			return
		}
		if m, err := s.store.OpenAppMove(ctx, id); err == nil && copying(m) {
			busy(app.Name)
			return
		}
		if same(app.OwnerID) {
			continue
		}
		jobs, err := s.store.ListCronJobs(ctx, store.CronFilter{AppID: id})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if app.OwnerID != nil {
			givers[*app.OwnerID] = true
		}
		add.apps++
		add.cron += len(jobs)
		if app.DiskBytes != nil {
			add.disk += *app.DiskBytes
		}
		addServer(app.ServerID)
		add.names = append(add.names, app.Name)
		change.Apps = append(change.Apps, id)
	}
	for _, id := range req.MailDomains {
		d, err := s.store.GetMailDomain(ctx, id)
		if err != nil || d.State == "deleting" {
			notFound("mail_not_found")
			return
		}
		if same(d.OwnerID) {
			continue
		}
		boxes, err := s.store.MailboxesOn(ctx, id, "")
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		add.mailDomains++
		addServer(d.ServerID)
		add.mailboxes += len(boxes)
		for _, b := range boxes {
			if b.UsedBytes != nil {
				add.disk += *b.UsedBytes
			}
		}
		add.names = append(add.names, d.Domain)
		change.MailDomains = append(change.MailDomains, id)
	}
	for _, id := range req.Zones {
		z, err := s.store.GetHostedZone(ctx, id)
		if err != nil {
			notFound("zone_not_found")
			return
		}
		if same(z.OwnerID) {
			continue
		}
		add.names = append(add.names, z.Name)
		change.Zones = append(change.Zones, id)
	}
	if len(add.names) == 0 {
		writeError(w, http.StatusBadRequest, "nothing_to_give", "the new owner has all of these already")
		return
	}
	if to != nil && !s.ownerChangeFits(w, r, target, add) {
		return
	}
	servers, err := s.store.ChangeOwner(ctx, change)
	if errors.Is(err, store.ErrNotFound) {
		notFound("not_found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	newOwner := "administrators"
	if to != nil {
		newOwner = target.Email
	}
	s.audit(r, "owner.changed", newOwner, map[string]any{"to": req.To, "names": add.names, "sites": change.Sites,
		"databases": change.Databases, "apps": change.Apps, "mail_domains": change.MailDomains, "zones": change.Zones})
	for _, id := range servers {
		s.queueReconcile(r, id)
	}
	if len(change.Zones) > 0 || len(change.Sites) > 0 || len(change.Apps) > 0 || len(change.MailDomains) > 0 {
		s.queueNameServers(r) // records follow the owner of the zone
	}
	// Traffic goes with websites and apps: both sides may be blocked or unblocked now.
	if to != nil && (len(change.Sites) > 0 || len(change.Apps) > 0) {
		givers[*to] = true
	}
	for id := range givers {
		s.opts.Tasks.CheckBandwidth(ctx, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"moved": len(add.names)})
}

// ownerChangeFits checks the new owner's package for everything an owner change adds.
func (s *Server) ownerChangeFits(w http.ResponseWriter, r *http.Request, target store.Customer, add ownerChangeTotals) bool {
	pkg, err := s.store.GetPackage(r.Context(), *target.PackageID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	u := target.Usage
	refuse := func(code, msg string) bool {
		writeError(w, http.StatusConflict, code, msg)
		return false
	}
	for _, id := range add.servers {
		if len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, id) {
			return refuse("new_owner_server", "the new owner's package cannot use the server of one of these")
		}
	}
	switch {
	case add.sites > 0 && u.Sites+add.sites > pkg.MaxSites:
		return refuse("new_owner_limit_site", "the new owner's package does not allow that many websites")
	case add.databases > 0 && u.Databases+add.databases > pkg.MaxDatabases:
		return refuse("new_owner_limit_database", "the new owner's package does not allow that many databases")
	case add.apps > 0 && u.Apps+add.apps > pkg.MaxApps:
		return refuse("new_owner_limit_app", "the new owner's package does not allow that many apps")
	case add.cron > 0 && u.CronJobs+add.cron > pkg.MaxCronJobs:
		return refuse("new_owner_limit_cron", "the new owner's package does not allow that many cron jobs")
	case add.mailDomains > 0 && u.MailDomains+add.mailDomains > pkg.MaxMailDomains:
		return refuse("new_owner_limit_mail_domain", "the new owner's package does not allow that many email domains")
	case add.mailboxes > 0 && u.Mailboxes+add.mailboxes > pkg.MaxMailboxes:
		return refuse("new_owner_limit_mailbox", "the new owner's package does not allow that many mailboxes")
	case add.disk > 0 && pkg.DiskMB != nil && u.DiskBytes+add.disk > int64(*pkg.DiskMB)<<20:
		return refuse("new_owner_quota", "these do not fit in the disk space of the new owner's package")
	}
	return true
}

// copying reports whether a move to another server is still under way; once it has
// switched, only the old copy waits to be removed and the owner can change.
func copying(m store.Move) bool { return m.Phase == "copying" || m.Phase == "switching" }

// urlSafeID returns id if it is a UUID, or an id that matches nothing.
func urlSafeID(id string) string {
	if isUUID(id) {
		return id
	}
	return nilUUID
}
