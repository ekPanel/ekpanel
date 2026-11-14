package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/publicsuffix"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// Built-in DNS: administrators choose which servers are name servers; administrators and
// customers keep zones on them (customers only for their own websites' and apps' domains).

type nameServerView struct {
	store.NameServer
	// The addresses glue records and zone transfers use.
	IPv4     string `json:"ipv4"`
	IPv6     string `json:"ipv6"`
	Transfer string `json:"transfer"`
}

func nameServerViews(list []store.NameServer) []nameServerView {
	out := make([]nameServerView, len(list))
	for i, n := range list {
		out[i] = nameServerView{NameServer: n, IPv4: tasks.PublicAddress(n.Target(), "A"), IPv6: tasks.PublicAddress(n.Target(), "AAAA"),
			Transfer: tasks.TransferAddress(n)}
	}
	return out
}

func (s *Server) handleListNameServers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListNameServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, nameServerViews(list))
}

type nameServerRequest struct {
	ServerID        string `json:"server_id"`
	Hostname        string `json:"hostname"`
	Primary         bool   `json:"primary"`
	TransferAddress string `json:"transfer_address"`
}

func (s *Server) handleSetNameServers(w http.ResponseWriter, r *http.Request) {
	var req []nameServerRequest
	if !readJSON(w, r, &req) {
		return
	}
	bad := func(msg string) { writeError(w, http.StatusBadRequest, "invalid_name_servers", msg) }
	var list []store.NameServer
	primaries := 0
	for _, n := range req {
		srv, err := s.store.GetServer(r.Context(), n.ServerID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && srv.EnrolledAt == nil) {
			bad("unknown server")
			return
		} else if err != nil {
			s.internalError(w, r, err)
			return
		}
		host := normalizeHostname(n.Hostname)
		if host == "" {
			bad("enter a host name such as ns1.example.com for " + srv.Name)
			return
		}
		if slices.ContainsFunc(list, func(x store.NameServer) bool { return x.ServerID == n.ServerID || x.Hostname == host }) {
			bad("each server and host name may be listed once")
			return
		}
		ns := store.NameServer{ServerID: srv.ID, ServerName: srv.Name, Hostname: host, Primary: n.Primary,
			PublicIPv4: srv.PublicIPv4, PublicIPv6: srv.PublicIPv6, Addresses: srv.Addresses}
		if a := strings.TrimSpace(n.TransferAddress); a != "" {
			ip, err := netip.ParseAddr(a)
			if err != nil || ip.Zone() != "" {
				bad("invalid transfer address " + a)
				return
			}
			v := ip.String()
			ns.TransferAddress = &v
		}
		if n.Primary {
			primaries++
		}
		list = append(list, ns)
	}
	if len(list) > 0 && primaries != 1 {
		bad("choose one primary name server")
		return
	}
	if len(list) > 1 {
		for _, n := range list {
			if tasks.TransferAddress(n) == "" {
				bad(n.ServerName + " has no public address for zone transfers; enter one")
				return
			}
		}
	}
	old, err := s.store.ListNameServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.SetNameServers(r.Context(), list); err != nil {
		s.internalError(w, r, err)
		return
	}
	hosts := make([]string, len(list))
	for i, n := range list {
		hosts[i] = n.Hostname
	}
	s.audit(r, "dns.name_servers", strings.Join(hosts, ", "), map[string]any{"count": len(list)})
	// Servers that stop being name servers are applied too, which stops PowerDNS there.
	for _, n := range append(old, list...) {
		s.queueReconcile(r, n.ServerID)
	}
	s.handleListNameServers(w, r)
}

// ---- Hosted zones ----

func (s *Server) getHostedZone(r *http.Request) (store.HostedZone, error) {
	z, err := s.store.GetHostedZone(r.Context(), chi.URLParam(r, "id"))
	if err == nil && !s.visible(r, z.OwnerID) {
		return store.HostedZone{}, store.ErrNotFound
	}
	return z, err
}

func (s *Server) writeHostedError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrZoneTaken):
		writeError(w, http.StatusConflict, "zone_taken", err.Error())
	case errors.Is(err, store.ErrRecordExists):
		writeError(w, http.StatusConflict, "record_exists", err.Error())
	case errors.Is(err, tasks.ErrNoNameServers):
		writeError(w, http.StatusConflict, "no_name_servers", err.Error())
	default:
		s.internalError(w, r, err)
	}
}

// queueNameServers applies every name server after a zone changed.
func (s *Server) queueNameServers(r *http.Request) {
	var by *string
	if u := currentUser(r); u != nil {
		by = &u.ID
	}
	s.opts.Tasks.QueueNameServers(r.Context(), by)
}

func (s *Server) handleListHostedZones(w http.ResponseWriter, r *http.Request) {
	zones, err := s.store.ListHostedZones(r.Context(), isAdmin(r), ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, zones)
}

type hostedZoneRequest struct {
	Name       string `json:"name"`
	TemplateID string `json:"template_id"`
	OwnerID    string `json:"owner_id"` // administrators: a customer's zone
}

func (s *Server) handleCreateHostedZone(w http.ResponseWriter, r *http.Request) {
	var req hostedZoneRequest
	if !readJSON(w, r, &req) {
		return
	}
	name := normalizeHostname(req.Name)
	if suffix, _ := publicsuffix.PublicSuffix(name); name == "" || !agentproto.ValidZoneName(name) || name == suffix {
		writeError(w, http.StatusBadRequest, "invalid_zone", "enter a domain such as example.com")
		return
	}
	ns, err := s.store.ListNameServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if !slices.ContainsFunc(ns, func(n store.NameServer) bool { return n.Primary }) {
		s.writeHostedError(w, r, tasks.ErrNoNameServers)
		return
	}
	u := currentUser(r)
	var owner *string
	if u.Role != "customer" {
		if u.Role == "reseller" && req.OwnerID == "" {
			writeError(w, http.StatusBadRequest, "customer_required", "choose the customer this is for")
			return
		}
		if req.OwnerID != "" {
			c, err := s.store.UserByID(r.Context(), urlSafeID(req.OwnerID))
			if err != nil || c.Role != "customer" || (u.Role == "reseller" && (c.ResellerID == nil || *c.ResellerID != u.ID)) {
				writeError(w, http.StatusBadRequest, "customer_not_found", "no such customer")
				return
			}
			owner = &c.ID
		}
	} else {
		owner = &u.ID
	}
	if u.Role != "admin" {
		// Customers keep zones only for the domains of their own websites and apps.
		hosts, err := s.store.OwnerHostnames(r.Context(), *owner)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !slices.ContainsFunc(hosts, func(h string) bool { return h == name || strings.HasSuffix(h, "."+name) }) {
			writeError(w, http.StatusForbidden, "zone_not_owned", "add a website or app on this domain first")
			return
		}
	}
	if taken, err := s.store.ZoneNameTaken(r.Context(), name); err != nil {
		s.internalError(w, r, err)
		return
	} else if taken {
		s.writeHostedError(w, r, store.ErrZoneTaken)
		return
	}
	// A zone inside or around someone else's would take over part of their domain.
	zones, err := s.store.ListHostedZones(r.Context(), true, "")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if z := zoneOverlap(zones, name, owner); z != nil {
		writeError(w, http.StatusConflict, "zone_overlaps", "this domain is inside, or contains, another account's zone "+z.Name)
		return
	}
	var records []agentproto.HostedRecord
	if req.TemplateID != "" {
		t, err := s.store.GetDNSTemplate(r.Context(), req.TemplateID)
		if err != nil {
			s.writeHostedError(w, r, err)
			return
		}
		for _, tr := range t.Records {
			tr.Name = absoluteName(tr.Name, name)
			tr.Content = strings.ReplaceAll(tr.Content, "{domain}", name)
			if err := tr.Check(name); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_template", t.Name+": "+err.Error())
				return
			}
			records = append(records, tr)
		}
	}
	z, err := s.store.CreateHostedZone(r.Context(), name, owner, &u.ID, records)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	s.audit(r, "dns.hosted_zone_added", z.Name, map[string]any{"zone_id": z.ID, "template_id": req.TemplateID})
	s.queueNameServers(r)
	writeJSON(w, http.StatusCreated, z)
}

// zoneOverlap returns another account's zone that name is inside of, or that is inside name:
// such a zone would take over part of their domain.
func zoneOverlap(zones []store.HostedZone, name string, owner *string) *store.HostedZone {
	for i, z := range zones {
		inside := strings.HasSuffix(name, "."+z.Name) || strings.HasSuffix(z.Name, "."+name)
		if inside && (owner == nil) != (z.OwnerID == nil) || inside && owner != nil && z.OwnerID != nil && *owner != *z.OwnerID {
			return &zones[i]
		}
	}
	return nil
}

// absoluteName turns "@", "" or "www" into a name in zone, as zone files do; names in the
// zone, and names ending with a dot, stay as they are.
func absoluteName(name, zone string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if absolute, ok := strings.CutSuffix(name, "."); ok {
		return absolute
	}
	switch {
	case name == "@" || name == "":
		return zone
	case name == zone || strings.HasSuffix(name, "."+zone):
		return name
	}
	return name + "." + zone
}

type hostedZoneView struct {
	store.HostedZone
	Kept        []store.HostedRecord      `json:"kept"`
	Made        []agentproto.HostedRecord `json:"made"`
	NameServers []nameServerView          `json:"name_servers"`
}

func (s *Server) handleGetHostedZone(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	kept, err := s.store.ListHostedRecords(r.Context(), z.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	view := hostedZoneView{HostedZone: z, Kept: kept, Made: []agentproto.HostedRecord{}}
	builds, err := s.opts.Tasks.HostedZones(r.Context(), z.ID)
	if err != nil && !errors.Is(err, tasks.ErrNoNameServers) {
		s.internalError(w, r, err)
		return
	}
	if len(builds) == 1 {
		view.Made = builds[0].Made
		if b, err := json.Marshal(builds[0].Issues); err == nil && string(b) != "null" {
			view.Issues = b
		}
	}
	ns, err := s.store.ListNameServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	view.NameServers = nameServerViews(ns)
	if !isAdmin(r) {
		// Customers see the name servers' names and public addresses, not their state.
		for i := range view.NameServers {
			view.NameServers[i].Status, view.NameServers[i].TransferAddress, view.NameServers[i].Transfer, view.NameServers[i].Addresses =
				nil, nil, "", nil
			view.NameServers[i].ServerID, view.NameServers[i].ServerName = "", ""
		}
	}
	writeJSON(w, http.StatusOK, view)
}

type hostedZoneChange struct {
	DNSSEC *bool `json:"dnssec"`
}

func (s *Server) handleUpdateHostedZone(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	var req hostedZoneChange
	if !readJSON(w, r, &req) {
		return
	}
	if req.DNSSEC != nil && *req.DNSSEC != z.DNSSEC {
		if err := s.store.SetHostedZoneDNSSEC(r.Context(), z.ID, *req.DNSSEC); err != nil {
			s.internalError(w, r, err)
			return
		}
		s.audit(r, "dns.hosted_dnssec", z.Name, map[string]any{"zone_id": z.ID, "on": *req.DNSSEC})
		s.queueNameServers(r)
	}
	s.handleGetHostedZone(w, r)
}

func (s *Server) handleDeleteHostedZone(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	if err := s.store.DeleteHostedZone(r.Context(), z.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.hosted_zone_removed", z.Name, map[string]any{"zone_id": z.ID})
	s.queueNameServers(r)
	w.WriteHeader(http.StatusNoContent)
}

// checkHostedRecord validates a record for zone z, with the zone's other records.
func (s *Server) checkHostedRecord(w http.ResponseWriter, r *http.Request, z store.HostedZone, rec *agentproto.HostedRecord, replacing string) bool {
	rec.Name = absoluteName(rec.Name, z.Name)
	if rec.TTL == 0 {
		rec.TTL = 3600
	}
	if err := rec.Check(z.Name); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_record", err.Error())
		return false
	}
	kept, err := s.store.ListHostedRecords(r.Context(), z.ID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	all := []agentproto.HostedRecord{*rec}
	for _, k := range kept {
		if k.ID != replacing {
			all = append(all, k.Spec())
		}
	}
	if err := agentproto.CheckRecords(z.Name, all); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_record", err.Error())
		return false
	}
	return true
}

func (s *Server) handleCreateHostedRecord(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	var rec agentproto.HostedRecord
	if !readJSON(w, r, &rec) || !s.checkHostedRecord(w, r, z, &rec, "") {
		return
	}
	out, err := s.store.AddHostedRecord(r.Context(), z.ID, rec)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	s.audit(r, "dns.hosted_record_added", z.Name, map[string]any{"zone_id": z.ID, "name": rec.Name, "type": rec.Type, "content": rec.Content})
	s.queueNameServers(r)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleUpdateHostedRecord(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	var rec agentproto.HostedRecord
	rid := chi.URLParam(r, "rid")
	if !readJSON(w, r, &rec) || !s.checkHostedRecord(w, r, z, &rec, rid) {
		return
	}
	if err := s.store.UpdateHostedRecord(r.Context(), z.ID, rid, rec); err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	s.audit(r, "dns.hosted_record_changed", z.Name, map[string]any{"zone_id": z.ID, "name": rec.Name, "type": rec.Type, "content": rec.Content})
	s.queueNameServers(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteHostedRecord(w http.ResponseWriter, r *http.Request) {
	z, err := s.getHostedZone(r)
	if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	if err := s.store.DeleteHostedRecord(r.Context(), z.ID, chi.URLParam(r, "rid")); err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	s.audit(r, "dns.hosted_record_removed", z.Name, map[string]any{"zone_id": z.ID, "record_id": chi.URLParam(r, "rid")})
	s.queueNameServers(r)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Templates ----

func (s *Server) handleListDNSTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListDNSTemplates(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSaveDNSTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.DNSTemplate
	if !readJSON(w, r, &t) {
		return
	}
	t.ID = chi.URLParam(r, "id")
	t.Name = strings.TrimSpace(t.Name)
	if t.Name == "" || len(t.Name) > 60 {
		writeError(w, http.StatusBadRequest, "invalid_template", "enter a name of up to 60 characters")
		return
	}
	if len(t.Records) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_template", "a template may have up to 100 records")
		return
	}
	// Records are checked as they would be in a zone; names stay relative.
	const sample = "example.com"
	for i, rec := range t.Records {
		rel := strings.ToLower(strings.TrimSpace(rec.Name))
		if rel == "" {
			rel = "@"
		}
		rec.Name = absoluteName(rel, sample)
		rec.Content = strings.ReplaceAll(rec.Content, "{domain}", sample)
		if rec.TTL == 0 {
			rec.TTL = 3600
		}
		if err := rec.Check(sample); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_template", err.Error())
			return
		}
		t.Records[i].Name, t.Records[i].Type, t.Records[i].TTL = rel, rec.Type, rec.TTL
	}
	created := t.ID == ""
	out, err := s.store.SaveDNSTemplate(r.Context(), t)
	if errors.Is(err, store.ErrTemplateNameTaken) {
		writeError(w, http.StatusConflict, "template_name_taken", err.Error())
		return
	} else if err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	s.audit(r, "dns.template_saved", out.Name, map[string]any{"template_id": out.ID, "records": len(out.Records)})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func (s *Server) handleDeleteDNSTemplate(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteDNSTemplate(r.Context(), chi.URLParam(r, "id")); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "dns.template_removed", chi.URLParam(r, "id"), nil)
	w.WriteHeader(http.StatusNoContent)
}
