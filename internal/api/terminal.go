package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/hub"
)

const (
	confirmFor        = 15 * time.Minute // a confirmed session may open terminals this long
	terminalIdle      = 30 * time.Minute // closed after this long without a keystroke
	terminalMaxLength = 8 * time.Hour
)

// handleConfirm checks the password (and the 2FA code, if on) again, so the session may open
// terminals for a while. A stolen session cookie alone is not enough for a root shell.
func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		TOTPCode string `json:"totp_code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	u := currentUser(r)
	key := "confirm:" + u.ID
	if s.limiter.Blocked(key) {
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts; try again in 15 minutes")
		return
	}
	if ok, err := auth.VerifyPassword(req.Password, u.PasswordHash); err != nil || !ok {
		s.limiter.Fail(key)
		writeError(w, http.StatusUnauthorized, "invalid_password", "the password is not correct")
		return
	}
	if u.TOTPEnabled {
		if req.TOTPCode == "" {
			writeError(w, http.StatusUnauthorized, "totp_required", "enter the code from your authenticator app")
			return
		}
		if u.TOTPSecret == nil || !auth.CheckTOTP(*u.TOTPSecret, strings.TrimSpace(req.TOTPCode)) {
			s.limiter.Fail(key)
			writeError(w, http.StatusUnauthorized, "invalid_totp", "the authentication code is not correct")
			return
		}
	}
	s.limiter.Reset(key)
	hash, _ := r.Context().Value(sessionHashKey).([]byte)
	until := time.Now().Add(confirmFor)
	if err := s.store.ConfirmSession(r.Context(), hash, until); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "auth.confirmed", u.Email, nil)
	writeJSON(w, http.StatusOK, map[string]any{"confirmed_until": until})
}

// terminalAllowed answers before the WebSocket upgrade, which a browser cannot read errors from.
func (s *Server) terminalAllowed(w http.ResponseWriter, r *http.Request) bool {
	u := currentUser(r)
	if u.ImpersonatorID != nil {
		writeError(w, http.StatusForbidden, "forbidden", "terminals are not available while acting as a customer")
		return false
	}
	if u.ConfirmedUntil == nil {
		writeError(w, http.StatusForbidden, "confirm_required", "enter your password again to open a terminal")
		return false
	}
	return true
}

func (s *Server) handleServerTerminal(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) || !s.terminalAllowed(w, r) {
		return
	}
	s.runTerminal(w, r, srv.ID, agentproto.TerminalParams{Kind: "server"}, map[string]any{"server_id": srv.ID, "kind": "server"}, srv.Name)
}

func (s *Server) handleAppTerminal(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	if !s.terminalAllowed(w, r) {
		return
	}
	service := r.URL.Query().Get("service")
	if service == "" {
		service = agentproto.AppServiceName
		if app.Service != nil {
			service = *app.Service
		}
	}
	if !serviceRE.MatchString(service) {
		writeError(w, http.StatusBadRequest, "invalid_service", "unknown service")
		return
	}
	s.runTerminal(w, r, app.ServerID, agentproto.TerminalParams{Kind: "container", App: app.Name, Service: service},
		map[string]any{"app_id": app.ID, "service": service, "kind": "container"}, app.Name+"/"+service)
}

// runTerminal bridges a browser WebSocket and a terminal on the agent. Keystrokes and output
// travel as binary messages; the browser sends {"type":"resize","cols":…,"rows":…} as text,
// and gets {"type":"exit","reason":…} when the shell ends.
func (s *Server) runTerminal(w http.ResponseWriter, r *http.Request, serverID string, p agentproto.TerminalParams, details map[string]any, target string) {
	cols, _ := strconv.Atoi(r.URL.Query().Get("cols"))
	rows, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	p.Cols, p.Rows = cols, rows

	// Accept's default origin check rejects pages of other sites.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithTimeout(r.Context(), terminalMaxLength)
	defer cancel()

	exit := func(reason string) {
		b, _ := json.Marshal(map[string]string{"type": "exit", "reason": reason})
		wctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		conn.Write(wctx, websocket.MessageText, b)
		conn.Close(websocket.StatusNormalClosure, "")
	}
	openCtx, openCancel := context.WithTimeout(ctx, 30*time.Second)
	stream, err := s.hub.OpenStream(openCtx, serverID, agentproto.OpTerminalOpen, func(id string) any {
		p.Stream = id
		return p
	})
	openCancel()
	if err != nil {
		var opErr *agentproto.OpError
		switch {
		case errors.Is(err, hub.ErrOffline):
			exit("agent_offline")
		case errors.As(err, &opErr):
			exit(opErr.Message)
		default:
			exit(err.Error())
		}
		return
	}
	defer stream.Close()
	started := time.Now()
	s.audit(r, "terminal.opened", target, details)
	var in, out atomic.Int64
	defer func() {
		d := map[string]any{"seconds": int(time.Since(started).Seconds()), "bytes_in": in.Load(), "bytes_out": out.Load()}
		for k, v := range details {
			d[k] = v
		}
		s.audit(r, "terminal.closed", target, d)
	}()

	// Browser → agent. Keystrokes and resizes count as activity for the idle limit.
	var lastInput atomic.Int64
	lastInput.Store(time.Now().UnixNano())
	idleCheck := time.NewTicker(time.Minute)
	defer idleCheck.Stop()
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			lastInput.Store(time.Now().UnixNano())
			if typ == websocket.MessageBinary {
				in.Add(int64(len(data)))
				if stream.Write(ctx, data) != nil {
					return
				}
				continue
			}
			var msg struct {
				Type string `json:"type"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				stream.Resize(ctx, msg.Cols, msg.Rows)
			}
		}
	}()

	// Agent → browser.
	for {
		select {
		case data, ok := <-stream.Output():
			if !ok {
				exit(stream.Reason())
				return
			}
			out.Add(int64(len(data)))
			wctx, c := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageBinary, data)
			c()
			if err != nil {
				return
			}
		case <-idleCheck.C:
			if time.Since(time.Unix(0, lastInput.Load())) >= terminalIdle {
				exit("idle")
				return
			}
		case <-ctx.Done():
			exit("time_limit")
			return
		case <-readDone:
			return // the browser went away
		}
	}
}
