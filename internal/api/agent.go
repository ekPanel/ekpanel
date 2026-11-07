package api

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/hub"
	"ekpanel/internal/pki"
	"ekpanel/internal/store"
)

// AgentHandler serves the agent listener (mutual TLS, see internal/pki). It is separate
// from Handler so browsers and agents never share an endpoint or a TLS configuration.
func (s *Server) AgentHandler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recoverer)
	r.Post(agentproto.EnrollPath, s.handleAgentEnroll)
	r.Post(agentproto.RenewPath, s.handleAgentRenew)
	r.Get(agentproto.ConnectPath, s.handleAgentConnect)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "no such agent endpoint")
	})
	return r
}

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
	var certPEM []byte
	srv, err := s.store.EnrollServer(r.Context(), auth.HashToken(strings.TrimSpace(req.Token)),
		func(serverID string) (string, time.Time, error) {
			pemBytes, fp, err := s.opts.CA.SignAgentCSR([]byte(req.CSRPEM), serverID)
			if err != nil {
				return "", time.Time{}, fmt.Errorf("%w: %v", errBadCSR, err)
			}
			certPEM = pemBytes
			return fp, time.Now().Add(pki.ClientValidity), nil
		})
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.limiter.Fail(ipKey)
		writeError(w, http.StatusForbidden, "invalid_enroll_token", "the enrolment token is wrong, used or expired; create a new one in the panel")
		return
	case errors.Is(err, errBadCSR):
		writeError(w, http.StatusBadRequest, "invalid_csr", err.Error())
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.auditAs(r, nil, "server.enrolled", srv.Name, map[string]any{"server_id": srv.ID})
	writeJSON(w, http.StatusOK, agentproto.EnrollResponse{ServerID: srv.ID, CertPEM: string(certPEM)})
}

var errBadCSR = errors.New("invalid certificate request")

// agentFromTLS identifies the agent by its verified client certificate.
func (s *Server) agentFromTLS(w http.ResponseWriter, r *http.Request) (store.Server, string, bool) {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "agent certificate required")
		return store.Server{}, "", false
	}
	leaf := r.TLS.PeerCertificates[0]
	if !hasClientAuth(leaf) {
		writeError(w, http.StatusUnauthorized, "invalid_agent_credentials", "not an agent certificate")
		return store.Server{}, "", false
	}
	id := leaf.Subject.CommonName
	if !isUUID(id) {
		id = nilUUID
	}
	fp := pki.Fingerprint(leaf.Raw)
	srv, err := s.store.AuthenticateAgent(r.Context(), id, fp)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "invalid_agent_credentials", "this agent certificate has been revoked; enrol the agent again")
		return store.Server{}, "", false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.Server{}, "", false
	}
	return srv, fp, true
}

func hasClientAuth(c *x509.Certificate) bool {
	for _, u := range c.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			return true
		}
	}
	return false
}

func (s *Server) handleAgentRenew(w http.ResponseWriter, r *http.Request) {
	srv, oldFP, ok := s.agentFromTLS(w, r)
	if !ok {
		return
	}
	var req agentproto.RenewRequest
	if !readJSON(w, r, &req) {
		return
	}
	certPEM, fp, err := s.opts.CA.SignAgentCSR([]byte(req.CSRPEM), srv.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_csr", err.Error())
		return
	}
	if err := s.store.ReplaceAgentCert(r.Context(), srv.ID, oldFP, fp, time.Now().Add(pki.ClientValidity)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusConflict, "invalid_agent_credentials", "the certificate changed during renewal; try again")
			return
		}
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, agentproto.RenewResponse{CertPEM: string(certPEM)})
}

// agentReadTimeout closes connections from agents that stop sending samples.
const agentReadTimeout = 30 * time.Second

func (s *Server) handleAgentConnect(w http.ResponseWriter, r *http.Request) {
	srv, _, ok := s.agentFromTLS(w, r)
	if !ok {
		return
	}
	// Agents are not browsers, so there is no Origin to check; mutual TLS authenticated them.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	ctx := r.Context()

	ac := &agentConn{conn: conn, pending: map[string]chan agentproto.Envelope{}, streams: map[string]*agentStream{}}
	defer ac.closeAll()
	disconnected := s.hub.Connected(srv.ID, ac)
	defer disconnected()
	s.log.Info("agent connected", "server", srv.Name, "id", srv.ID, "ip", clientIP(r))
	defer s.log.Info("agent disconnected", "server", srv.Name, "id", srv.ID)

	minute := &minuteAgg{}
	defer func() { s.flushMinute(srv.ID, minute) }()
	var lastTouch time.Time
	for {
		readCtx, cancel := context.WithTimeout(ctx, agentReadTimeout)
		_, data, err := conn.Read(readCtx)
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
			err = s.store.UpdateServerFacts(ctx, srv.ID, store.ServerFacts{
				Hostname: trim(h.Hostname, 255), OS: trim(h.OS, 255), Kernel: trim(h.Kernel, 255),
				Arch: trim(h.Arch, 32), CPUCount: h.CPUCount, AgentVersion: trim(h.AgentVersion, 64),
			})
			lastTouch = time.Now()
			// The agent may have missed changes while it was offline.
			if s.opts.Tasks != nil {
				go func() {
					if _, err := s.opts.Tasks.Reconcile(context.WithoutCancel(ctx), srv.ID); err != nil {
						s.log.Warn("reconcile after connect", "server", srv.ID, "err", err)
					}
				}()
			}
		case env.Type == agentproto.TypeMetrics && env.Metrics != nil:
			s.hub.Publish(srv.ID, *env.Metrics)
			// Buckets use the panel's clock, so a wrong clock on the server cannot scramble history.
			if now := time.Now(); minute.samples > 0 && !now.Truncate(time.Minute).Equal(minute.bucket) {
				s.flushMinute(srv.ID, minute)
			}
			minute.add(time.Now(), env.Metrics)
			if time.Since(lastTouch) > 30*time.Second {
				err = s.store.TouchServer(ctx, srv.ID)
				lastTouch = time.Now()
			}
		case env.Type == agentproto.TypeResponse:
			ac.deliver(env)
		case env.Type == agentproto.TypeStreamData || env.Type == agentproto.TypeStreamClose:
			ac.deliverStream(env)
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

// ---- Operations sent to an agent ----

// agentConn implements hub.Caller for one agent WebSocket.
type agentConn struct {
	conn    *websocket.Conn
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan agentproto.Envelope
	streams map[string]*agentStream
	closed  bool
}

// OpError is returned by Call when the agent ran the operation and it failed.
type OpError = agentproto.OpError

var errAgentGone = errors.New("the agent disconnected before answering")

func (a *agentConn) Call(ctx context.Context, op string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := strconv.FormatUint(a.nextID.Add(1), 10)
	ch := make(chan agentproto.Envelope, 1)
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errAgentGone
	}
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	b, _ := json.Marshal(agentproto.Envelope{Type: agentproto.TypeRequest, ID: id, Op: op, Params: raw})
	if err := a.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, errAgentGone
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (a *agentConn) deliver(env agentproto.Envelope) {
	a.mu.Lock()
	ch := a.pending[env.ID]
	a.mu.Unlock()
	if ch != nil {
		select {
		case ch <- env:
		default: // a duplicate response; the first one already answered the call
		}
	}
}

func (a *agentConn) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	for id, ch := range a.pending {
		close(ch)
		delete(a.pending, id)
	}
	for id, st := range a.streams {
		st.end("the server's agent disconnected")
		delete(a.streams, id)
	}
}

// ---- Streams (terminals) ----

// streamBuffer is how many chunks may wait for a slow browser before the stream is dropped,
// so one stuck viewer cannot hold up the agent's whole connection.
const streamBuffer = 512

type agentStream struct {
	id     string
	conn   *agentConn
	out    chan []byte
	mu     sync.Mutex // guards ended, so data is never pushed into a closed channel
	ended  bool
	reason string
}

func (st *agentStream) Output() <-chan []byte { return st.out }
func (st *agentStream) Reason() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.reason
}

func (st *agentStream) end(reason string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.ended {
		st.ended, st.reason = true, reason
		close(st.out)
	}
}

// push queues data for the browser; false means the stream has ended or the buffer is full.
func (st *agentStream) push(data []byte) (ok bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.ended {
		return true // nothing to do, and nothing wrong with the connection
	}
	select {
	case st.out <- data:
		return true
	default:
		return false
	}
}

func (st *agentStream) send(ctx context.Context, env agentproto.Envelope) error {
	env.Stream = st.id
	b, _ := json.Marshal(env)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return st.conn.conn.Write(ctx, websocket.MessageText, b)
}

func (st *agentStream) Write(ctx context.Context, data []byte) error {
	for len(data) > 0 {
		n := min(len(data), agentproto.MaxStreamChunk)
		if err := st.send(ctx, agentproto.Envelope{Type: agentproto.TypeStreamData, Data: data[:n]}); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (st *agentStream) Resize(ctx context.Context, cols, rows int) error {
	return st.send(ctx, agentproto.Envelope{Type: agentproto.TypeStreamResize, Cols: cols, Rows: rows})
}

// Close ends the stream from the panel's side; the agent stops the shell.
func (st *agentStream) Close() {
	st.send(context.Background(), agentproto.Envelope{Type: agentproto.TypeStreamClose, Reason: "closed"})
	st.conn.mu.Lock()
	delete(st.conn.streams, st.id)
	st.conn.mu.Unlock()
	st.end("closed")
}

func (a *agentConn) OpenStream(ctx context.Context, op string, params func(id string) any) (hub.Stream, error) {
	id := "s" + strconv.FormatUint(a.nextID.Add(1), 10)
	st := &agentStream{id: id, conn: a, out: make(chan []byte, streamBuffer)}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, errAgentGone
	}
	a.streams[id] = st
	a.mu.Unlock()
	if _, err := a.Call(ctx, op, params(id)); err != nil {
		a.mu.Lock()
		delete(a.streams, id)
		a.mu.Unlock()
		return nil, err
	}
	return st, nil
}

func (a *agentConn) deliverStream(env agentproto.Envelope) {
	a.mu.Lock()
	st := a.streams[env.Stream]
	if st != nil && env.Type == agentproto.TypeStreamClose {
		delete(a.streams, env.Stream)
	}
	a.mu.Unlock()
	if st == nil {
		return
	}
	if env.Type == agentproto.TypeStreamClose {
		st.end(env.Reason)
		return
	}
	if !st.push(env.Data) {
		a.mu.Lock()
		delete(a.streams, env.Stream)
		a.mu.Unlock()
		st.send(context.Background(), agentproto.Envelope{Type: agentproto.TypeStreamClose, Reason: "the browser could not keep up"})
		st.end("the browser could not keep up")
	}
}

// ---- Metric history ----

// minuteAgg averages one minute of samples before it is written to PostgreSQL.
type minuteAgg struct {
	bucket              time.Time
	samples             int
	cpu, load1, rx, tx  float64
	memUsed, memTotal   uint64
	diskUsed, diskTotal uint64
}

func (m *minuteAgg) add(now time.Time, x *agentproto.Metrics) {
	if m.samples == 0 {
		m.bucket = now.Truncate(time.Minute)
	}
	m.samples++
	m.cpu += x.CPUPercent
	m.load1 += x.Load1
	m.rx += x.NetRxRate
	m.tx += x.NetTxRate
	m.memUsed, m.memTotal, m.diskUsed, m.diskTotal = x.MemUsed, x.MemTotal, x.DiskUsed, x.DiskTotal
}

func (s *Server) flushMinute(serverID string, m *minuteAgg) {
	if m.samples == 0 {
		return
	}
	n := float64(m.samples)
	p := store.MetricPoint{
		At: m.bucket, CPUPercent: m.cpu / n, Load1: m.load1 / n, NetRxRate: m.rx / n, NetTxRate: m.tx / n,
		MemUsed: int64(m.memUsed), MemTotal: int64(m.memTotal), DiskUsed: int64(m.diskUsed), DiskTotal: int64(m.diskTotal),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.SaveMinute(ctx, serverID, p, m.samples); err != nil {
		s.log.Error("save metrics", "server", serverID, "err", err)
	}
	*m = minuteAgg{}
}
