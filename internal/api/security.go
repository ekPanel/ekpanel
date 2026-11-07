package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// Security of servers is for administrators: CrowdSec, its bans and the overview.

type securityView struct {
	store.ServerSecurity
	// Always are the addresses the agent never bans besides the allowlist.
	Always []string `json:"always"`
}

func (s *Server) handleGetSecurity(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	sec, err := s.store.GetSecurity(r.Context(), srv.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, securityView{ServerSecurity: sec, Always: append(slices.Clone(srv.Addresses), agentproto.CloudflareRanges...)})
}

type securityRequest struct {
	CrowdSec  bool     `json:"crowdsec"`
	Allowlist []string `json:"allowlist"`
}

func (s *Server) handleSetSecurity(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	var req securityRequest
	if !readJSON(w, r, &req) {
		return
	}
	list := []string{}
	for _, e := range req.Allowlist {
		n := agentproto.NormalizeAllowEntry(e)
		if n == "" {
			writeError(w, http.StatusBadRequest, "invalid_allowlist", "use IP addresses or networks such as 203.0.113.0/24: "+e)
			return
		}
		if !slices.Contains(list, n) {
			list = append(list, n)
		}
	}
	if len(list) > agentproto.MaxAllowlist {
		writeError(w, http.StatusBadRequest, "invalid_allowlist", "at most 200 entries")
		return
	}
	if err := s.store.SetSecurity(r.Context(), srv.ID, req.CrowdSec, list); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "security.set", srv.Name, map[string]any{"server_id": srv.ID, "crowdsec": req.CrowdSec, "allowlist": len(list)})
	s.queueReconcile(r, srv.ID)
	s.handleGetSecurity(w, r)
}

// securityCall runs a security operation on the server's agent and writes its answer.
func (s *Server) securityCall(w http.ResponseWriter, r *http.Request, op string, params any, timeout time.Duration) (json.RawMessage, bool) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	raw, err := s.hub.Call(ctx, srv.ID, op, params)
	if err != nil {
		s.writeAgentError(w, r, err)
		return nil, false
	}
	return raw, true
}

func (s *Server) handleSecurityOverview(w http.ResponseWriter, r *http.Request) {
	raw, ok := s.securityCall(w, r, agentproto.OpSecurityOverview, struct{}{}, 60*time.Second)
	if !ok {
		return
	}
	var o agentproto.SecurityOverview
	if err := json.Unmarshal(raw, &o); err != nil {
		s.internalError(w, r, err)
		return
	}
	admins, err := s.store.AdminsWithout2FA(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": o, "admins_without_2fa": admins})
}

func (s *Server) handleSecurityBans(w http.ResponseWriter, r *http.Request) {
	if raw, ok := s.securityCall(w, r, agentproto.OpSecurityBans, struct{}{}, 30*time.Second); ok {
		writeJSON(w, http.StatusOK, raw)
	}
}

func (s *Server) handleSecurityAlerts(w http.ResponseWriter, r *http.Request) {
	if raw, ok := s.securityCall(w, r, agentproto.OpSecurityAlerts, struct{}{}, 30*time.Second); ok {
		writeJSON(w, http.StatusOK, raw)
	}
}

func (s *Server) handleSecurityBan(w http.ResponseWriter, r *http.Request) {
	var p agentproto.BanParams
	if !readJSON(w, r, &p) {
		return
	}
	if err := p.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_ban", err.Error())
		return
	}
	if raw, ok := s.securityCall(w, r, agentproto.OpSecurityBan, p, 30*time.Second); ok {
		s.audit(r, "security.ban", p.IP, map[string]any{"server_id": serverID(r), "duration": p.Duration, "reason": p.Reason})
		writeJSON(w, http.StatusOK, raw)
	}
}

func (s *Server) handleSecurityUnban(w http.ResponseWriter, r *http.Request) {
	ip := agentproto.NormalizeAllowEntry(chi.URLParam(r, "ip"))
	if ip == "" {
		writeError(w, http.StatusBadRequest, "invalid_ban", "invalid address")
		return
	}
	if raw, ok := s.securityCall(w, r, agentproto.OpSecurityUnban, agentproto.UnbanParams{IP: ip}, 30*time.Second); ok {
		s.audit(r, "security.unban", ip, map[string]any{"server_id": serverID(r)})
		writeJSON(w, http.StatusOK, raw)
	}
}

func (s *Server) handleSecurityAutoUpdates(w http.ResponseWriter, r *http.Request) {
	var p agentproto.AutoUpdatesParams
	if !readJSON(w, r, &p) {
		return
	}
	raw, ok := s.securityCall(w, r, agentproto.OpSecurityAutoUpdates, p, 5*time.Minute)
	if !ok {
		return
	}
	s.audit(r, "security.auto_updates", serverID(r), map[string]any{"on": p.On})
	var o agentproto.SecurityOverview
	json.Unmarshal(raw, &o)
	admins, err := s.store.AdminsWithout2FA(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": o, "admins_without_2fa": admins})
}
