package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/cloudflare"
	"ekpanel/internal/store"
)

// Cloudflare accounts and the zones ekPanel keeps in sync. Customers see and manage only
// their own; administrators manage theirs and see every zone.

func cfAccountID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

// getCFAccount loads an account the user may use: their own, or the administrators' for
// an administrator.
func (s *Server) getCFAccount(r *http.Request, id string) (store.CloudflareAccount, error) {
	a, err := s.store.GetCloudflareAccount(r.Context(), id)
	if err != nil {
		return a, err
	}
	u := currentUser(r)
	if (u.Role == "admin" && a.OwnerID != nil) || (u.Role != "admin" && (a.OwnerID == nil || *a.OwnerID != u.ID)) {
		return store.CloudflareAccount{}, store.ErrNotFound
	}
	return a, nil
}

func (s *Server) getZone(r *http.Request, id string) (store.DNSZone, error) {
	z, err := s.store.GetZone(r.Context(), id)
	if err == nil && !visible(r, z.OwnerID) {
		return store.DNSZone{}, store.ErrNotFound
	}
	return z, err
}

func (s *Server) writeDNSError(w http.ResponseWriter, r *http.Request, err error) {
	var cf *cloudflare.Error
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrZoneTaken):
		writeError(w, http.StatusConflict, "zone_taken", err.Error())
	case cloudflare.IsAuth(err):
		writeError(w, http.StatusBadGateway, "cloudflare_denied", "Cloudflare refused the token: "+err.Error())
	case errors.As(err, &cf):
		writeError(w, http.StatusBadGateway, "cloudflare_error", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) cfClient(r *http.Request, a store.CloudflareAccount) (*cloudflare.Client, error) {
	token, err := s.opts.Secrets.Open(a.Token)
	if err != nil {
		return nil, err
	}
	return cloudflare.New(string(token)), nil
}

func cfContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 60*time.Second)
}

func (s *Server) handleListCFAccounts(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListCloudflareAccounts(r.Context(), ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleConnectCloudflare checks a token with Cloudflare and keeps it, sealed.
func (s *Server) handleConnectCloudflare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Token string `json:"token"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	req.Name, req.Token = strings.TrimSpace(req.Name), strings.TrimSpace(req.Token)
	if req.Token == "" || len(req.Token) > 200 || strings.ContainsAny(req.Token, " \n\t") {
		writeError(w, http.StatusBadRequest, "invalid_token", "paste the API token Cloudflare showed when you created it")
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	c := cloudflare.New(req.Token)
	if err := c.Verify(ctx); err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	a := store.CloudflareAccount{Name: req.Name, Token: s.opts.Secrets.Seal([]byte(req.Token))}
	// Adding new domains needs the account; a token limited to zones may not list it.
	if accounts, err := c.Accounts(ctx); err == nil && len(accounts) > 0 {
		a.AccountID, a.AccountName = &accounts[0].ID, &accounts[0].Name
		if a.Name == "" {
			a.Name = accounts[0].Name
		}
	}
	if a.Name == "" {
		a.Name = "Cloudflare"
	}
	if len(a.Name) > 100 {
		a.Name = a.Name[:100]
	}
	u := currentUser(r)
	if u.Role != "admin" {
		a.OwnerID = &u.ID
	}
	a, err := s.store.CreateCloudflareAccount(r.Context(), a, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.cloudflare_connected", a.Name, map[string]any{"account_id": a.ID})
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) handleDisconnectCloudflare(w http.ResponseWriter, r *http.Request) {
	a, err := s.getCFAccount(r, cfAccountID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	if err := s.store.DeleteCloudflareAccount(r.Context(), a.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.cloudflare_disconnected", a.Name, map[string]any{"account_id": a.ID})
	w.WriteHeader(http.StatusNoContent)
}

// handleCloudflareZones lists the account's zones and which of them ekPanel manages.
func (s *Server) handleCloudflareZones(w http.ResponseWriter, r *http.Request) {
	a, err := s.getCFAccount(r, cfAccountID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	c, err := s.cfClient(r, a)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	zones, err := c.Zones(ctx)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	managed, err := s.store.ListZones(r.Context(), true, "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	type entry struct {
		cloudflare.Zone
		ManagedID *string `json:"managed_id"` // the ekPanel zone, if it manages this one
	}
	out := []entry{}
	for _, z := range zones {
		e := entry{Zone: z}
		for _, m := range managed {
			if m.CFZoneID == z.ID && m.AccountID == a.ID {
				e.ManagedID = &m.ID
			}
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListZones(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListZones(r.Context(), isAdmin(r), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleAddZone starts managing a zone of a connected account in one click: by its
// Cloudflare ID, or (with a name) after adding the domain to Cloudflare.
func (s *Server) handleAddZone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccountID string `json:"account_id"`
		CFZoneID  string `json:"cf_zone_id"`
		Name      string `json:"name"`
		Proxied   *bool  `json:"proxied"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !isUUID(req.AccountID) {
		writeError(w, http.StatusNotFound, "not_found", "choose a connected Cloudflare account")
		return
	}
	a, err := s.getCFAccount(r, req.AccountID)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	c, err := s.cfClient(r, a)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	var cz cloudflare.Zone
	if req.CFZoneID != "" {
		cz, err = c.Zone(ctx, req.CFZoneID)
	} else {
		name := normalizeHostname(req.Name)
		if name == "" || !strings.Contains(name, ".") {
			writeError(w, http.StatusBadRequest, "invalid_domain", "enter a domain such as example.com")
			return
		}
		if a.AccountID == nil {
			writeError(w, http.StatusBadRequest, "cloudflare_account_unknown", "this token cannot see its Cloudflare account; give it Account Settings: Read, or add the domain in Cloudflare first")
			return
		}
		cz, err = c.CreateZone(ctx, *a.AccountID, name)
	}
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	z := store.DNSZone{AccountID: a.ID, OwnerID: a.OwnerID, CFZoneID: cz.ID, Name: strings.ToLower(cz.Name), Status: cz.Status,
		NameServers: cz.NameServers, Proxied: req.Proxied == nil || *req.Proxied}
	if z.NameServers == nil {
		z.NameServers = []string{}
	}
	// A domain is kept either at Cloudflare or on ekPanel's own name servers, not both.
	if taken, err := s.store.ZoneNameTaken(r.Context(), z.Name); err != nil {
		s.internalError(w, r, err)
		return
	} else if taken {
		s.writeDNSError(w, r, store.ErrZoneTaken)
		return
	}
	z, err = s.store.CreateZone(r.Context(), z)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.zone_added", z.Name, map[string]any{"zone_id": z.ID, "created_in_cloudflare": req.CFZoneID == ""})
	s.queueDNSSync(r, z.ID)
	writeJSON(w, http.StatusCreated, z)
}

func (s *Server) queueDNSSync(r *http.Request, zoneID string) {
	var by *string
	if u := currentUser(r); u != nil {
		by = &u.ID
	}
	if _, err := s.opts.Tasks.QueueDNSSync(r.Context(), zoneID, by); err != nil {
		s.log.Error("queue dns sync", "err", err)
	}
}

func zoneID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

// zoneClient loads a zone the user may see and a client for its account.
func (s *Server) zoneClient(w http.ResponseWriter, r *http.Request) (store.DNSZone, *cloudflare.Client, bool) {
	z, err := s.getZone(r, zoneID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return z, nil, false
	}
	c, err := s.opts.Tasks.CloudflareClient(r.Context(), z.AccountID)
	if err != nil {
		s.internalError(w, r, err)
		return z, nil, false
	}
	return z, c, true
}

// handleGetZone returns the zone with every record in Cloudflare, marking those ekPanel
// made, and the zone's SSL mode.
func (s *Server) handleGetZone(w http.ResponseWriter, r *http.Request) {
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	records, err := c.Records(ctx, z.CFZoneID)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	managed, err := s.store.ManagedRecords(r.Context(), z.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	type record struct {
		cloudflare.Record
		Managed bool `json:"managed"`
	}
	out := []record{}
	for _, rec := range records {
		out = append(out, record{Record: rec, Managed: slices.ContainsFunc(managed, func(m store.DNSRecord) bool { return m.CFRecordID == rec.ID })})
	}
	slices.SortFunc(out, func(a, b record) int { return strings.Compare(a.Name+" "+a.Type, b.Name+" "+b.Type) })
	ssl, _ := c.SSLMode(ctx, z.CFZoneID) // a token without zone settings permission cannot read it
	writeJSON(w, http.StatusOK, map[string]any{"zone": z, "records": out, "ssl_mode": ssl})
}

func (s *Server) handleUpdateZone(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Proxied bool `json:"proxied"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	z, err := s.getZone(r, zoneID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	if err := s.store.SetZoneProxied(r.Context(), z.ID, req.Proxied); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.zone_updated", z.Name, map[string]any{"zone_id": z.ID, "proxied": req.Proxied})
	z, _ = s.store.GetZone(r.Context(), z.ID)
	writeJSON(w, http.StatusOK, z)
}

// handleRemoveZone stops managing a zone. With ?records=delete, the records ekPanel made
// are deleted from Cloudflare first, except those someone changed since; otherwise they stay.
func (s *Server) handleRemoveZone(w http.ResponseWriter, r *http.Request) {
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("records") == "delete" {
		managed, err := s.store.ManagedRecords(r.Context(), z.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		ctx, cancel := cfContext(r)
		defer cancel()
		have, err := c.Records(ctx, z.CFZoneID)
		if err != nil {
			s.writeDNSError(w, r, err)
			return
		}
		for _, m := range managed {
			i := slices.IndexFunc(have, func(rec cloudflare.Record) bool { return rec.ID == m.CFRecordID })
			if i < 0 || have[i].Content != m.Content || have[i].Proxied != m.Proxied {
				continue // gone already, or changed by hand: not ekPanel's to delete
			}
			if err := c.DeleteRecord(ctx, z.CFZoneID, m.CFRecordID); err != nil && !isCFNotFound(err) {
				s.writeDNSError(w, r, err)
				return
			}
		}
	}
	if err := s.store.DeleteZone(r.Context(), z.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.zone_removed", z.Name, map[string]any{"zone_id": z.ID, "records_deleted": r.URL.Query().Get("records") == "delete"})
	w.WriteHeader(http.StatusNoContent)
}

func isCFNotFound(err error) bool {
	var e *cloudflare.Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (s *Server) handleSyncZone(w http.ResponseWriter, r *http.Request) {
	z, err := s.getZone(r, zoneID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.queueDNSSync(r, z.ID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// handleResolveIssue writes ekPanel's record over what is in the way, now.
func (s *Server) handleResolveIssue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	z, err := s.getZone(r, zoneID(r))
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	if err := s.opts.Tasks.SyncZones(r.Context(), z.ID, map[string]bool{req.Type + " " + req.Name: true}); err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.issue_resolved", z.Name, map[string]any{"zone_id": z.ID, "record": req.Type + " " + req.Name})
	z, _ = s.store.GetZone(r.Context(), z.ID)
	writeJSON(w, http.StatusOK, z)
}

// ---- Zone editor ----

var editableTypes = []string{"A", "AAAA", "CNAME", "MX", "TXT", "CAA", "NS"}

// checkRecord validates a record for the zone; names may be relative ("www", "@").
func checkRecord(z store.DNSZone, rec *cloudflare.Record) error {
	rec.Type = strings.ToUpper(strings.TrimSpace(rec.Type))
	if !slices.Contains(editableTypes, rec.Type) {
		return fmt.Errorf("records of type %s cannot be edited here", rec.Type)
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(rec.Name), "."))
	switch {
	case name == "" || name == "@":
		name = z.Name
	case name != z.Name && !strings.HasSuffix(name, "."+z.Name):
		name += "." + z.Name
	}
	if normalizeHostname(strings.TrimPrefix(name, "*.")) == "" {
		return errors.New("enter a name such as www, @ or a full host name in the zone")
	}
	rec.Name = name
	rec.Content = strings.TrimSpace(rec.Content)
	ip := net.ParseIP(rec.Content)
	switch rec.Type {
	case "A":
		if ip == nil || ip.To4() == nil {
			return errors.New("an A record points at an IPv4 address")
		}
	case "AAAA":
		if ip == nil || ip.To4() != nil {
			return errors.New("an AAAA record points at an IPv6 address")
		}
	case "CNAME", "NS", "MX":
		if normalizeHostname(strings.TrimSuffix(rec.Content, ".")) == "" {
			return fmt.Errorf("a %s record points at a host name", rec.Type)
		}
	case "TXT", "CAA":
		if rec.Content == "" || len(rec.Content) > 4096 || strings.ContainsAny(rec.Content, "\n\r") {
			return errors.New("enter the record's text on one line")
		}
	}
	if rec.Type == "MX" && (rec.Priority == nil || *rec.Priority < 0 || *rec.Priority > 65535) {
		return errors.New("an MX record needs a priority from 0 to 65535")
	}
	if rec.Type != "MX" {
		rec.Priority = nil
	}
	if rec.TTL != 1 && (rec.TTL < 60 || rec.TTL > 86400) {
		rec.TTL = 1
	}
	if rec.Type != "A" && rec.Type != "AAAA" && rec.Type != "CNAME" {
		rec.Proxied = false
	}
	if len(rec.Comment) > 100 {
		rec.Comment = rec.Comment[:100]
	}
	return nil
}

func (s *Server) handleCreateRecord(w http.ResponseWriter, r *http.Request) {
	var rec cloudflare.Record
	if !readJSON(w, r, &rec) {
		return
	}
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	if err := checkRecord(z, &rec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_record", err.Error())
		return
	}
	rec.ID = ""
	ctx, cancel := cfContext(r)
	defer cancel()
	out, err := c.CreateRecord(ctx, z.CFZoneID, rec)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.record_created", z.Name, map[string]any{"zone_id": z.ID, "record": out.Type + " " + out.Name})
	writeJSON(w, http.StatusCreated, out)
}

// managedRecord returns the record ekPanel made with that ID, if it is one.
func (s *Server) managedRecord(r *http.Request, z store.DNSZone, id string) (*store.DNSRecord, error) {
	managed, err := s.store.ManagedRecords(r.Context(), z.ID)
	if err != nil {
		return nil, err
	}
	for i := range managed {
		if managed[i].CFRecordID == id {
			return &managed[i], nil
		}
	}
	return nil, nil
}

// handleUpdateRecord changes a record. Of a record ekPanel made, only "proxied" changes;
// the rest follows the website or app it belongs to.
func (s *Server) handleUpdateRecord(w http.ResponseWriter, r *http.Request) {
	var rec cloudflare.Record
	if !readJSON(w, r, &rec) {
		return
	}
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "rid")
	m, err := s.managedRecord(r, z, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if m != nil {
		rec = cloudflare.Record{Type: m.Type, Name: m.Name, Content: m.Content, TTL: 1, Proxied: rec.Proxied, Comment: cloudflare.ManagedComment}
	} else if err := checkRecord(z, &rec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_record", err.Error())
		return
	}
	rec.ID = id
	ctx, cancel := cfContext(r)
	defer cancel()
	out, err := c.UpdateRecord(ctx, z.CFZoneID, rec)
	if err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	if m != nil {
		if err := s.store.KeepRecord(r.Context(), z.ID, store.DNSRecord{CFRecordID: out.ID, Type: out.Type, Name: out.Name, Content: out.Content, Proxied: out.Proxied}); err != nil {
			s.internalError(w, r, err)
			return
		}
	}
	s.audit(r, "dns.record_updated", z.Name, map[string]any{"zone_id": z.ID, "record": out.Type + " " + out.Name})
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteRecord(w http.ResponseWriter, r *http.Request) {
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "rid")
	m, err := s.managedRecord(r, z, id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if m != nil {
		writeError(w, http.StatusConflict, "managed_record", "ekPanel made this record for a website or app; it goes when that does")
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	if err := c.DeleteRecord(ctx, z.CFZoneID, id); err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.record_deleted", z.Name, map[string]any{"zone_id": z.ID, "record_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleZoneSSL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if !slices.Contains([]string{"off", "flexible", "full", "strict"}, req.Mode) {
		writeError(w, http.StatusBadRequest, "invalid_ssl_mode", "choose off, flexible, full or strict")
		return
	}
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	if err := c.SetSSLMode(ctx, z.CFZoneID, req.Mode); err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.ssl_mode", z.Name, map[string]any{"zone_id": z.ID, "mode": req.Mode})
	writeJSON(w, http.StatusOK, map[string]string{"ssl_mode": req.Mode})
}

func (s *Server) handlePurgeZone(w http.ResponseWriter, r *http.Request) {
	z, c, ok := s.zoneClient(w, r)
	if !ok {
		return
	}
	ctx, cancel := cfContext(r)
	defer cancel()
	if err := c.PurgeCache(ctx, z.CFZoneID); err != nil {
		s.writeDNSError(w, r, err)
		return
	}
	s.audit(r, "dns.cache_purged", z.Name, map[string]any{"zone_id": z.ID})
	writeJSON(w, http.StatusOK, map[string]string{"status": "purged"})
}

// ---- Servers' public addresses ----

func (s *Server) handleSetServerAddresses(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IPv4 string `json:"public_ipv4"`
		IPv6 string `json:"public_ipv6"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	parse := func(v string, four bool) (*string, bool) {
		if v = strings.TrimSpace(v); v == "" {
			return nil, true
		}
		ip := net.ParseIP(v)
		if ip == nil || (ip.To4() != nil) != four {
			return nil, false
		}
		out := ip.String()
		return &out, true
	}
	v4, ok4 := parse(req.IPv4, true)
	v6, ok6 := parse(req.IPv6, false)
	if !ok4 || !ok6 {
		writeError(w, http.StatusBadRequest, "invalid_address", "enter an IPv4 and an IPv6 address, or leave them empty to find them automatically")
		return
	}
	srv, err := s.store.SetServerAddresses(r.Context(), serverID(r), v4, v6)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	s.audit(r, "server.addresses_set", srv.Name, map[string]any{"server_id": srv.ID, "ipv4": v4, "ipv6": v6})
	s.queueDNSSync(r, "")
	writeJSON(w, http.StatusOK, srv)
}
