package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

func (s *Server) writeMailStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "mail_not_found", "not found")
	case errors.Is(err, store.ErrMailHostTaken):
		writeError(w, http.StatusConflict, "mail_host_taken", err.Error())
	case errors.Is(err, store.ErrMailDomainTaken):
		writeError(w, http.StatusConflict, "mail_domain_taken", err.Error())
	case errors.Is(err, store.ErrMailAddressTaken):
		writeError(w, http.StatusConflict, "mail_address_taken", err.Error())
	case errors.Is(err, store.ErrInUse):
		writeError(w, http.StatusConflict, "mail_in_use", "delete the server's mail domains first")
	default:
		s.internalError(w, r, err)
	}
}

// ---- Mail servers (administrators) ----

type mailServerRequest struct {
	Hostname string `json:"hostname"`
	TLS      string `json:"tls"`
}

// handleListMailServers lists the servers with email; customers see those their package
// may use.
func (s *Server) handleListMailServers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListMailServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !isAdmin(r) {
		place, ok := s.resolveOwner(w, r, "")
		if !ok {
			return
		}
		list = slices.DeleteFunc(list, func(m store.MailServer) bool {
			return len(place.Package.ServerIDs) > 0 && !slices.Contains(place.Package.ServerIDs, m.ServerID)
		})
		for i := range list {
			list[i].Message, list[i].Domains = nil, 0
		}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSetMailServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	var req mailServerRequest
	if !readJSON(w, r, &req) {
		return
	}
	host := normalizeHostname(strings.ToLower(req.Hostname))
	if host == "" || strings.Count(host, ".") < 1 {
		writeError(w, http.StatusBadRequest, "invalid_hostname", "enter a host name such as mail.example.com")
		return
	}
	if req.TLS == "" {
		req.TLS = "auto"
	}
	if req.TLS != "auto" && req.TLS != "internal" {
		writeError(w, http.StatusBadRequest, "invalid_tls", "choose a certificate type")
		return
	}
	// Webmail is a website at the host name, so no other website or app may use it.
	if owners, err := s.store.HostnameOwners(r.Context(), []string{host}); err != nil {
		s.internalError(w, r, err)
		return
	} else if len(owners) > 0 {
		writeError(w, http.StatusConflict, "hostname_taken", "a website or app already uses this host name")
		return
	}
	ms, err := s.store.SetMailServer(r.Context(), srv.ID, host, req.TLS)
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.server_set", srv.Name, map[string]any{"server_id": srv.ID, "hostname": host})
	s.queueReconcile(r, srv.ID)
	writeJSON(w, http.StatusOK, ms)
}

func (s *Server) handleDeleteMailServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	if err := s.store.DeleteMailServer(r.Context(), srv.ID); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.server_removed", srv.Name, map[string]any{"server_id": srv.ID})
	s.queueReconcile(r, srv.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleMailCheck asks the server how other mail servers will see it.
func (s *Server) handleMailCheck(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.GetMailServer(r.Context(), serverID(r))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, ms.ServerID, agentproto.OpMailCheck, agentproto.MailCheckParams{Hostname: ms.Hostname})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	var res agentproto.MailCheckResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.internalError(w, r, err)
		return
	}
	srv, err := s.store.GetServer(r.Context(), ms.ServerID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// The addresses DNS should point at, as the DNS sync sees them.
	target := store.HostTarget{Hostname: ms.Hostname, ServerName: srv.Name, Addresses: srv.Addresses, PublicIPv4: srv.PublicIPv4, PublicIPv6: srv.PublicIPv6}
	public := []string{}
	for _, typ := range []string{"A", "AAAA"} {
		if a := tasks.PublicAddress(target, typ); a != "" {
			public = append(public, a)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"check": res, "public_addresses": public})
}

// ---- Mail domains ----

type mailDomainRequest struct {
	ServerID string `json:"server_id"`
	Domain   string `json:"domain"`
	OwnerID  string `json:"owner_id"`
}

func (s *Server) getMailDomain(r *http.Request, id string) (store.MailDomain, error) {
	d, err := s.store.GetMailDomain(r.Context(), id)
	if err == nil && !visible(r, d.OwnerID) {
		return store.MailDomain{}, store.ErrNotFound
	}
	return d, err
}

func pathID(r *http.Request, name string) string {
	if id := chi.URLParam(r, name); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) handleListMailDomains(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListMailDomains(r.Context(), "", ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateMailDomain(w http.ResponseWriter, r *http.Request) {
	var req mailDomainRequest
	if !readJSON(w, r, &req) {
		return
	}
	domain := normalizeHostname(strings.ToLower(req.Domain))
	if domain == "" {
		writeError(w, http.StatusBadRequest, "invalid_domain", "enter a domain such as example.com")
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	ms, err := s.store.GetMailServer(r.Context(), req.ServerID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusBadRequest, "mail_not_enabled", "this server has no email")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if domain == ms.Hostname {
		writeError(w, http.StatusBadRequest, "invalid_domain", "the mail host name cannot be a mail domain")
		return
	}
	place, ok := s.resolveOwner(w, r, req.OwnerID)
	if !ok || !s.allowNew(w, r, place, "mail_domain", ms.ServerID) {
		return
	}
	// A customer may only take mail for a domain of their own websites or apps: the
	// domain itself, or the domain of its www. name.
	if place.OwnerID != nil {
		owners, err := s.store.HostnameOwners(r.Context(), []string{domain, "www." + domain})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !slices.ContainsFunc(owners, func(o *string) bool { return o != nil && *o == *place.OwnerID }) {
			writeError(w, http.StatusForbidden, "mail_domain_not_owned", "add a website with this domain first")
			return
		}
	}
	u := currentUser(r)
	d, err := s.store.CreateMailDomain(r.Context(), store.NewMailDomain{ServerID: ms.ServerID, Domain: domain, OwnerID: place.OwnerID, CreatedBy: &u.ID})
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.domain_created", d.Domain, map[string]any{"mail_domain_id": d.ID, "server_id": ms.ServerID})
	s.queueReconcile(r, ms.ServerID)
	writeJSON(w, http.StatusCreated, d)
}

// mailDomainDetail is a domain with its addresses, and the Cloudflare zone its records
// are published to (nil: they must be added at the domain's DNS host).
type mailDomainDetail struct {
	Domain     store.MailDomain      `json:"domain"`
	Mailboxes  []store.Mailbox       `json:"mailboxes"`
	Forwarders []store.MailForwarder `json:"forwarders"`
	Zone       *store.DNSZone        `json:"zone"`
}

func (s *Server) handleGetMailDomain(w http.ResponseWriter, r *http.Request) {
	d, err := s.getMailDomain(r, pathID(r, "id"))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	out := mailDomainDetail{Domain: d}
	if out.Mailboxes, err = s.store.MailboxesOn(r.Context(), d.ID, ""); err != nil {
		s.internalError(w, r, err)
		return
	}
	if out.Forwarders, err = s.store.ForwardersOn(r.Context(), d.ID, ""); err != nil {
		s.internalError(w, r, err)
		return
	}
	zones, err := s.store.ListZones(r.Context(), true, "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if z := tasks.ZoneFor(d.Domain, d.OwnerID, zones); z != nil && visible(r, z.OwnerID) {
		out.Zone = z
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUpdateMailDomain(w http.ResponseWriter, r *http.Request) {
	d, err := s.getMailDomain(r, pathID(r, "id"))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	var req struct {
		CatchAll *string `json:"catch_all"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.CatchAll != nil && *req.CatchAll == "" {
		req.CatchAll = nil
	}
	if req.CatchAll != nil {
		boxes, err := s.store.MailboxesOn(r.Context(), d.ID, "")
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !slices.ContainsFunc(boxes, func(b store.Mailbox) bool { return b.LocalPart == *req.CatchAll }) {
			writeError(w, http.StatusBadRequest, "invalid_catch_all", "choose one of the domain's mailboxes")
			return
		}
	}
	if d, err = s.store.SetMailCatchAll(r.Context(), d.ID, req.CatchAll); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.catch_all_set", d.Domain, map[string]any{"mail_domain_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleDeleteMailDomain(w http.ResponseWriter, r *http.Request) {
	d, err := s.getMailDomain(r, pathID(r, "id"))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	if d, err = s.store.MarkMailDomainDeleting(r.Context(), d.ID); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.domain_deleted", d.Domain, map[string]any{"mail_domain_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusAccepted, d)
}

// ---- Mailboxes ----

type mailboxRequest struct {
	LocalPart string                `json:"local_part"`
	Password  string                `json:"password"`
	QuotaMB   *int                  `json:"quota_mb"`
	ForwardTo *[]string             `json:"forward_to"`
	KeepCopy  *bool                 `json:"keep_copy"`
	AutoReply *agentproto.AutoReply `json:"auto_reply"`
	// ClearAutoReply turns the auto-reply off.
	ClearAutoReply bool `json:"clear_auto_reply"`
}

// mailboxWithPassword is returned when the panel chose the password; it is shown only this once.
type mailboxWithPassword struct {
	Mailbox  store.Mailbox `json:"mailbox"`
	Password string        `json:"password,omitempty"`
}

// mailPackage is the package limiting a domain's owner, or nil.
func (s *Server) mailPackage(ctx context.Context, d store.MailDomain) (*store.Package, error) {
	if d.OwnerID == nil {
		return nil, nil
	}
	c, err := s.store.GetCustomer(ctx, *d.OwnerID)
	if err != nil {
		return nil, err
	}
	p, err := s.store.GetPackage(ctx, *c.PackageID)
	return &p, err
}

// applyMailboxRequest validates the request and copies it onto m. With a new mailbox,
// fields left out get their defaults.
func applyMailboxRequest(w http.ResponseWriter, req mailboxRequest, m *store.Mailbox, pkg *store.Package) bool {
	fail := func(code, msg string) bool {
		writeError(w, http.StatusBadRequest, code, msg)
		return false
	}
	if req.QuotaMB != nil {
		m.QuotaMB = *req.QuotaMB
	}
	if m.QuotaMB < 0 || m.QuotaMB > agentproto.MaxMailboxQuotaMB {
		return fail("invalid_quota", "enter a size in MB, or 0 for no limit")
	}
	if pkg != nil && pkg.MailboxQuotaMB != nil && (m.QuotaMB == 0 || m.QuotaMB > *pkg.MailboxQuotaMB) {
		return fail("quota_too_large", "the package allows mailboxes of up to its mailbox size")
	}
	if req.ForwardTo != nil {
		to := []string{}
		for _, a := range *req.ForwardTo {
			if a = strings.TrimSpace(a); a != "" && !slices.Contains(to, a) {
				to = append(to, a)
			}
		}
		if err := agentproto.ValidateForwards(to); err != nil {
			return fail("invalid_forward", err.Error())
		}
		m.ForwardTo = to
	}
	if req.KeepCopy != nil {
		m.KeepCopy = *req.KeepCopy
	}
	if !m.KeepCopy && len(m.ForwardTo) == 0 {
		return fail("invalid_forward", "a mailbox that keeps no copy must forward its mail")
	}
	if req.ClearAutoReply {
		m.AutoReply = nil
	} else if req.AutoReply != nil {
		a := *req.AutoReply
		a.Subject = strings.TrimSpace(a.Subject)
		if err := a.Validate(); err != nil {
			return fail("invalid_auto_reply", err.Error())
		}
		m.AutoReply = &a
	}
	return true
}

// mailboxPassword checks a chosen password, or makes one when none is given.
func mailboxPassword(w http.ResponseWriter, password string) (pw, hash string, generated, ok bool) {
	if password == "" {
		password, generated = auth.NewDatabasePassword(), true
	}
	if len([]rune(password)) < auth.MinPasswordLength || len(password) > 1024 {
		writeError(w, http.StatusBadRequest, "weak_password", "use a password of at least 10 characters")
		return "", "", false, false
	}
	h, err := auth.MailPasswordHash(password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not hash the password")
		return "", "", false, false
	}
	return password, h, generated, true
}

func (s *Server) handleCreateMailbox(w http.ResponseWriter, r *http.Request) {
	d, err := s.getMailDomain(r, pathID(r, "id"))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	var req mailboxRequest
	if !readJSON(w, r, &req) {
		return
	}
	local := strings.ToLower(strings.TrimSpace(req.LocalPart))
	if !agentproto.ValidLocalPart(local) {
		writeError(w, http.StatusBadRequest, "invalid_local_part", "use letters, digits and . _ + - , starting and ending with a letter or digit")
		return
	}
	pkg, err := s.mailPackage(r.Context(), d)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if pkg != nil {
		usage, err := s.store.OwnerUsage(r.Context(), *d.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if usage.Mailboxes >= pkg.MaxMailboxes {
			writeError(w, http.StatusConflict, "limit_reached_mailbox", "the package allows no more of these")
			return
		}
	}
	m := store.Mailbox{DomainID: d.ID, LocalPart: local, KeepCopy: true}
	if pkg != nil && pkg.MailboxQuotaMB != nil {
		m.QuotaMB = *pkg.MailboxQuotaMB
	}
	if !applyMailboxRequest(w, req, &m, pkg) {
		return
	}
	password, hash, generated, ok := mailboxPassword(w, req.Password)
	if !ok {
		return
	}
	m.PasswordHash = hash
	if m, err = s.store.CreateMailbox(r.Context(), m); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.mailbox_created", local+"@"+d.Domain, map[string]any{"mailbox_id": m.ID, "mail_domain_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	out := mailboxWithPassword{Mailbox: m}
	if generated {
		out.Password = password
	}
	writeJSON(w, http.StatusCreated, out)
}

// loadMailbox finds a mailbox and its domain, if the user may see them.
func (s *Server) loadMailbox(w http.ResponseWriter, r *http.Request) (store.Mailbox, store.MailDomain, bool) {
	m, err := s.store.GetMailbox(r.Context(), pathID(r, "id"))
	if err == nil {
		var d store.MailDomain
		if d, err = s.getMailDomain(r, m.DomainID); err == nil {
			return m, d, true
		}
	}
	s.writeMailStoreError(w, r, err)
	return store.Mailbox{}, store.MailDomain{}, false
}

func (s *Server) handleUpdateMailbox(w http.ResponseWriter, r *http.Request) {
	m, d, ok := s.loadMailbox(w, r)
	if !ok {
		return
	}
	var req mailboxRequest
	if !readJSON(w, r, &req) {
		return
	}
	pkg, err := s.mailPackage(r.Context(), d)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !applyMailboxRequest(w, req, &m, pkg) {
		return
	}
	var password string
	var generated bool
	if req.Password != "" {
		var hash string
		if password, hash, generated, ok = mailboxPassword(w, req.Password); !ok {
			return
		}
		m.PasswordHash = hash
	}
	if m, err = s.store.SaveMailbox(r.Context(), m); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.mailbox_updated", m.LocalPart+"@"+d.Domain, map[string]any{"mailbox_id": m.ID, "password_changed": req.Password != ""})
	s.queueReconcile(r, d.ServerID)
	out := mailboxWithPassword{Mailbox: m}
	if generated {
		out.Password = password
	}
	writeJSON(w, http.StatusOK, out)
}

// handleResetMailboxPassword sets a new random password and shows it once.
func (s *Server) handleResetMailboxPassword(w http.ResponseWriter, r *http.Request) {
	m, d, ok := s.loadMailbox(w, r)
	if !ok {
		return
	}
	password, hash, _, ok := mailboxPassword(w, "")
	if !ok {
		return
	}
	m.PasswordHash = hash
	m, err := s.store.SaveMailbox(r.Context(), m)
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.mailbox_password_reset", m.LocalPart+"@"+d.Domain, map[string]any{"mailbox_id": m.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusOK, mailboxWithPassword{Mailbox: m, Password: password})
}

func (s *Server) handleDeleteMailbox(w http.ResponseWriter, r *http.Request) {
	m, d, ok := s.loadMailbox(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMailbox(r.Context(), m); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.mailbox_deleted", m.LocalPart+"@"+d.Domain, map[string]any{"mailbox_id": m.ID})
	s.queueReconcile(r, d.ServerID)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Forwarders ----

type forwarderRequest struct {
	LocalPart    string   `json:"local_part"`
	Destinations []string `json:"destinations"`
}

func cleanDestinations(w http.ResponseWriter, in []string) ([]string, bool) {
	out := []string{}
	for _, a := range in {
		if a = strings.TrimSpace(a); a != "" && !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_forward", "add at least one address")
		return nil, false
	}
	if err := agentproto.ValidateForwards(out); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_forward", err.Error())
		return nil, false
	}
	return out, true
}

func (s *Server) handleCreateForwarder(w http.ResponseWriter, r *http.Request) {
	d, err := s.getMailDomain(r, pathID(r, "id"))
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	var req forwarderRequest
	if !readJSON(w, r, &req) {
		return
	}
	local := strings.ToLower(strings.TrimSpace(req.LocalPart))
	if !agentproto.ValidLocalPart(local) {
		writeError(w, http.StatusBadRequest, "invalid_local_part", "use letters, digits and . _ + - , starting and ending with a letter or digit")
		return
	}
	to, ok := cleanDestinations(w, req.Destinations)
	if !ok {
		return
	}
	f, err := s.store.CreateMailForwarder(r.Context(), store.MailForwarder{DomainID: d.ID, LocalPart: local, Destinations: to})
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.forwarder_created", local+"@"+d.Domain, map[string]any{"forwarder_id": f.ID, "mail_domain_id": d.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusCreated, f)
}

func (s *Server) loadForwarder(w http.ResponseWriter, r *http.Request) (store.MailForwarder, store.MailDomain, bool) {
	f, err := s.store.GetMailForwarder(r.Context(), pathID(r, "id"))
	if err == nil {
		var d store.MailDomain
		if d, err = s.getMailDomain(r, f.DomainID); err == nil {
			return f, d, true
		}
	}
	s.writeMailStoreError(w, r, err)
	return store.MailForwarder{}, store.MailDomain{}, false
}

func (s *Server) handleUpdateForwarder(w http.ResponseWriter, r *http.Request) {
	f, d, ok := s.loadForwarder(w, r)
	if !ok {
		return
	}
	var req forwarderRequest
	if !readJSON(w, r, &req) {
		return
	}
	if f.Destinations, ok = cleanDestinations(w, req.Destinations); !ok {
		return
	}
	f, err := s.store.SaveMailForwarder(r.Context(), f)
	if err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.forwarder_updated", f.LocalPart+"@"+d.Domain, map[string]any{"forwarder_id": f.ID})
	s.queueReconcile(r, d.ServerID)
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleDeleteForwarder(w http.ResponseWriter, r *http.Request) {
	f, d, ok := s.loadForwarder(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteMailForwarder(r.Context(), f); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	s.audit(r, "mail.forwarder_deleted", f.LocalPart+"@"+d.Domain, map[string]any{"forwarder_id": f.ID})
	s.queueReconcile(r, d.ServerID)
	w.WriteHeader(http.StatusNoContent)
}
