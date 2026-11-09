package api

import (
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/store"
)

func (s *Server) handleGetFirewall(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	fw, err := s.store.GetFirewall(r.Context(), id)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, fw)
}

func (s *Server) applyFirewall(w http.ResponseWriter, r *http.Request, serverID string) bool {
	u := currentUser(r)
	if _, err := s.opts.Tasks.QueueReconcile(r.Context(), serverID, &u.ID); err != nil {
		s.internalError(w, r, err)
		return false
	}
	fw, err := s.store.GetFirewall(r.Context(), serverID)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	writeJSON(w, http.StatusOK, fw)
	return true
}

// handleSetFirewall turns ekPanel's firewall on or off for a server. "unmanaged" hands the
// firewall back as it is.
func (s *Server) handleSetFirewall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string `json:"mode"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Mode != "on" && req.Mode != "off" && req.Mode != "unmanaged" {
		writeError(w, http.StatusBadRequest, "invalid_firewall_mode", "choose on, off or unmanaged")
		return
	}
	id := serverID(r)
	err := s.store.SetFirewallMode(r.Context(), id, req.Mode)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	s.audit(r, "firewall.mode_changed", req.Mode, map[string]any{"server_id": id, "mode": req.Mode})
	s.applyFirewall(w, r, id)
}

type firewallRuleRequest struct {
	Action   string `json:"action"`
	Protocol string `json:"protocol"`
	Ports    string `json:"ports"` // "8080" or "6000-6010"; empty blocks every port
	Source   string `json:"source"`
	Note     string `json:"note"`
}

// parsePorts reads "8080" or "6000-6010". An empty string means every port.
func parsePorts(s string) (from, to *int, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil, true
	}
	a, b, isRange := strings.Cut(s, "-")
	if !isRange {
		b = a
	}
	pa, okA := portNumber(a)
	pb, okB := portNumber(b)
	if !okA || !okB || pb < pa {
		return nil, nil, false
	}
	return &pa, &pb, true
}

func portNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 5 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, n >= 1 && n <= 65535
}

// parseSource accepts an address or CIDR and returns its canonical form.
func parseSource(s string) (netip.Prefix, string, bool) {
	s = strings.TrimSpace(s)
	p, err := netip.ParsePrefix(s)
	if err != nil {
		a, aerr := netip.ParseAddr(s)
		if aerr != nil || a.Zone() != "" {
			return p, "", false
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	p = p.Masked()
	if p.IsSingleIP() {
		return p, p.Addr().String(), true
	}
	return p, p.String(), true
}

func (s *Server) handleAddFirewallRule(w http.ResponseWriter, r *http.Request) {
	var req firewallRuleRequest
	if !readJSON(w, r, &req) {
		return
	}
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	if req.Action != "allow" && req.Action != "deny" {
		writeError(w, http.StatusBadRequest, "invalid_firewall_rule", "choose to open a port or block an address")
		return
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" && req.Protocol != "both" {
		writeError(w, http.StatusBadRequest, "invalid_protocol", "choose TCP, UDP or both")
		return
	}
	from, to, ok := parsePorts(req.Ports)
	if !ok || (req.Action == "allow" && from == nil) {
		writeError(w, http.StatusBadRequest, "invalid_ports", "enter a port such as 8080, or a range such as 6000-6010")
		return
	}
	source := ""
	if strings.TrimSpace(req.Source) != "" || req.Action == "deny" {
		prefix, canonical, ok := parseSource(req.Source)
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_source", "enter an IP address or a range such as 203.0.113.0/24")
			return
		}
		// Blocking yourself would cut off the panel (and SSH) for the person doing it.
		if req.Action == "deny" {
			if me, err := netip.ParseAddr(clientIP(r)); err == nil && prefix.Contains(me.Unmap()) {
				writeError(w, http.StatusBadRequest, "firewall_blocks_you", "that would block your own address")
				return
			}
		}
		source = canonical
	}
	req.Note = strings.TrimSpace(req.Note)
	if utf8.RuneCountInString(req.Note) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_note", "keep the note under 200 characters")
		return
	}
	u := currentUser(r)
	rule, err := s.store.AddFirewallRule(r.Context(), store.NewFirewallRule{
		ServerID: id, Action: req.Action, Protocol: req.Protocol, PortFrom: from, PortTo: to, Source: source, Note: req.Note, CreatedBy: &u.ID,
	})
	if errors.Is(err, store.ErrLimit) {
		writeError(w, http.StatusConflict, "too_many_firewall_rules", "a server can have up to 200 rules")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "firewall.rule_added", firewallRuleText(rule), map[string]any{"server_id": id, "rule_id": rule.ID})
	s.applyFirewall(w, r, id)
}

func (s *Server) handleDeleteFirewallRule(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	ruleID := chi.URLParam(r, "rule")
	if !isUUID(ruleID) {
		ruleID = nilUUID
	}
	rule, err := s.store.DeleteFirewallRule(r.Context(), id, ruleID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "firewall_rule_not_found", "no such rule on this server")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "firewall.rule_removed", firewallRuleText(rule), map[string]any{"server_id": id, "rule_id": rule.ID})
	s.applyFirewall(w, r, id)
}

// firewallRuleText is a short description for the audit log, such as "allow 8080/tcp".
func firewallRuleText(r store.FirewallRule) string {
	var b strings.Builder
	b.WriteString(r.Action)
	if r.PortFrom != nil {
		b.WriteString(" ")
		b.WriteString(strconv.Itoa(*r.PortFrom))
		if *r.PortTo != *r.PortFrom {
			b.WriteString("-" + strconv.Itoa(*r.PortTo))
		}
		b.WriteString("/" + r.Protocol)
	}
	if r.Source != "" {
		b.WriteString(" from " + r.Source)
	}
	return b.String()
}
