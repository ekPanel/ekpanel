package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// serverView is a server plus its live state, as the UI lists it.
type serverView struct {
	store.Server
	Online bool                `json:"online"`
	Latest *agentproto.Metrics `json:"latest"`
}

func (s *Server) view(srv store.Server) serverView {
	online, latest := s.hub.Latest(srv.ID)
	return serverView{Server: srv, Online: online, Latest: latest}
}

func (s *Server) handleListServers(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]serverView, 0, len(list))
	for _, srv := range list {
		out = append(out, s.view(srv))
	}
	writeJSON(w, http.StatusOK, out)
}

type createServerRequest struct {
	Name string `json:"name"`
}

// enrollResponse carries the one-time token. It is shown once and never stored in clear.
type enrollResponse struct {
	Server      serverView `json:"server"`
	EnrollToken string     `json:"enroll_token"`
}

func (s *Server) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	var req createServerRequest
	if !readJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > 100 {
		writeError(w, http.StatusBadRequest, "invalid_name", "enter a name of up to 100 characters")
		return
	}
	token := auth.NewToken()
	srv, err := s.store.CreateServer(r.Context(), name, auth.HashToken(token))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "server.created", srv.Name, map[string]any{"server_id": srv.ID})
	writeJSON(w, http.StatusCreated, enrollResponse{Server: s.view(srv), EnrollToken: token})
}

func (s *Server) handleGetServer(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, s.view(srv))
}

func (s *Server) serverLookupFailed(w http.ResponseWriter, r *http.Request, err error) bool {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "server_not_found", "no server with that ID")
		return true
	}
	if err != nil {
		s.internalError(w, r, err)
		return true
	}
	return false
}

func (s *Server) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	srv, err := s.store.GetServer(r.Context(), id)
	if s.serverLookupFailed(w, r, err) {
		return
	}
	if err := s.store.DeleteServer(r.Context(), id); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.hub.Forget(id)
	s.audit(r, "server.deleted", srv.Name, map[string]any{"server_id": id})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenewEnrollToken(w http.ResponseWriter, r *http.Request) {
	token := auth.NewToken()
	srv, err := s.store.RenewEnrollToken(r.Context(), serverID(r), auth.HashToken(token))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	s.audit(r, "server.enroll_token_renewed", srv.Name, map[string]any{"server_id": srv.ID})
	writeJSON(w, http.StatusOK, enrollResponse{Server: s.view(srv), EnrollToken: token})
}

// handleServerLive streams a snapshot of recent metrics, then live events, to the browser.
func (s *Server) handleServerLive(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	// Accept's default origin check rejects cross-site pages.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := conn.CloseRead(r.Context())

	events, unsubscribe := s.hub.Subscribe(id)
	defer unsubscribe()

	online, history := s.hub.Snapshot(id)
	if err := writeWS(ctx, conn, map[string]any{"type": "snapshot", "online": online, "history": history}); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-events:
			if err := writeWS(ctx, conn, e); err != nil {
				return
			}
		}
	}
}

func writeWS(ctx context.Context, conn *websocket.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

const nilUUID = "00000000-0000-0000-0000-000000000000"

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func isUUID(s string) bool { return uuidPattern.MatchString(s) }

// serverID returns the {id} URL parameter. A malformed ID becomes the nil UUID, which
// matches no server, so lookups answer "not found" instead of a database type error.
func serverID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

// ---- Agent endpoints ----

func (s *Server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	var req agentproto.EnrollRequest
	if !readJSON(w, r, &req) {
		return
	}
	ipKey := "enroll:" + clientIP(r)
	if s.limiter.Blocked(ipKey) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many attempts; try again later")
		return
	}
	secret := auth.NewToken()
	srv, err := s.store.EnrollServer(r.Context(), auth.HashToken(strings.TrimSpace(req.Token)), auth.HashToken(secret))
	if errors.Is(err, store.ErrNotFound) {
		s.limiter.Fail(ipKey)
		writeError(w, http.StatusForbidden, "invalid_enroll_token", "the enrolment token is wrong, used or expired; create a new one in the panel")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.auditAs(r, nil, "server.enrolled", srv.Name, map[string]any{"server_id": srv.ID})
	writeJSON(w, http.StatusOK, agentproto.EnrollResponse{ServerID: srv.ID, Secret: secret})
}

// agentReadTimeout closes connections from agents that stop sending samples.
const agentReadTimeout = 30 * time.Second

func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := strings.Cut(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "agent credentials required")
		return
	}
	if !isUUID(id) {
		id = nilUUID
	}
	srv, err := s.store.AuthenticateAgent(r.Context(), id, auth.HashToken(secret))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid_agent_credentials", "agent credentials are not valid; enrol the agent again")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	// Agents are not browsers, so there is no Origin to check.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)

	disconnected := s.hub.Connected(srv.ID)
	defer disconnected()
	s.log.Info("agent connected", "server", srv.Name, "id", srv.ID, "ip", clientIP(r))
	defer s.log.Info("agent disconnected", "server", srv.Name, "id", srv.ID)

	var lastTouch time.Time
	for {
		ctx, cancel := context.WithTimeout(r.Context(), agentReadTimeout)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		var env agentproto.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			conn.Close(websocket.StatusUnsupportedData, "invalid message")
			return
		}
		switch {
		case env.Type == agentproto.TypeHello && env.Hello != nil:
			h := env.Hello
			err = s.store.UpdateServerFacts(r.Context(), srv.ID, store.ServerFacts{
				Hostname: trim(h.Hostname, 255), OS: trim(h.OS, 255), Kernel: trim(h.Kernel, 255),
				Arch: trim(h.Arch, 32), CPUCount: h.CPUCount, AgentVersion: trim(h.AgentVersion, 64),
			})
			lastTouch = time.Now()
		case env.Type == agentproto.TypeMetrics && env.Metrics != nil:
			s.hub.Publish(srv.ID, *env.Metrics)
			if time.Since(lastTouch) > 30*time.Second {
				err = s.store.TouchServer(r.Context(), srv.ID)
				lastTouch = time.Now()
			}
		}
		if err != nil {
			s.log.Error("agent message", "server", srv.ID, "err", err)
		}
	}
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- Audit log ----

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	entries, err := s.store.ListAudit(r.Context(), before, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
