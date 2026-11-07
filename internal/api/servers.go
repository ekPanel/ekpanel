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
// The UI builds the agent command from it: the panel's host name plus AgentPort, and the
// CA pin that lets the agent check it is talking to this panel.
type enrollResponse struct {
	Server      serverView `json:"server"`
	EnrollToken string     `json:"enroll_token"`
	CAPin       string     `json:"ca_pin"`
	AgentPort   int        `json:"agent_port"`
}

func (s *Server) enrollResponse(srv store.Server, token string) enrollResponse {
	return enrollResponse{Server: s.view(srv), EnrollToken: token, CAPin: s.opts.CA.Pin(), AgentPort: s.opts.AgentPort}
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
	writeJSON(w, http.StatusCreated, s.enrollResponse(srv, token))
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
	writeJSON(w, http.StatusOK, s.enrollResponse(srv, token))
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
