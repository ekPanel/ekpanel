package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
)

// moveAgent is a fake agent that applies state, and exports or imports a transfer of a
// fixed size with flow control, like a real one.
type moveAgent struct {
	t        *testing.T
	conn     *websocket.Conn
	mu       sync.Mutex
	applies  []agentproto.StateApplyParams
	received int
	credit   chan struct{}
	finish   *agentproto.OpError // what OpTransferFinish answers
	finished []agentproto.TransferFinishParams
	discards int
}

const transferChunks = 600 // more than the window, so flow control matters

func newMoveAgent(t *testing.T, p *testPanel, cfg agent.Config) *moveAgent {
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	a := &moveAgent{t: t, conn: conn, credit: make(chan struct{}, 10000)}
	a.send(agentproto.Envelope{Type: agentproto.TypeHello, Hello: &agentproto.Hello{Hostname: "h", AgentVersion: "v1"}})
	go a.loop()
	t.Cleanup(func() { conn.CloseNow() })
	return a
}

func (a *moveAgent) send(env agentproto.Envelope) {
	b, _ := json.Marshal(env)
	a.conn.Write(context.Background(), websocket.MessageText, b)
}

func (a *moveAgent) lastApply() agentproto.StateApplyParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.applies) == 0 {
		return agentproto.StateApplyParams{}
	}
	return a.applies[len(a.applies)-1]
}

func (a *moveAgent) loop() {
	importStream := ""
	for {
		_, data, err := a.conn.Read(context.Background())
		if err != nil {
			return
		}
		var env agentproto.Envelope
		json.Unmarshal(data, &env)
		switch env.Type {
		case agentproto.TypeStreamCredit:
			for range env.Credit {
				a.credit <- struct{}{}
			}
			continue
		case agentproto.TypeStreamData:
			a.mu.Lock()
			a.received++
			a.mu.Unlock()
			a.send(agentproto.Envelope{Type: agentproto.TypeStreamCredit, Stream: env.Stream, Credit: 1})
			continue
		case agentproto.TypeStreamClose:
			if env.Stream == importStream && env.Reason == "eof" {
				a.mu.Lock()
				n := a.received
				a.mu.Unlock()
				res, _ := json.Marshal(agentproto.TransferResult{Bytes: int64(n), Files: true})
				a.send(agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: env.Stream, Reason: "done", Result: res})
			}
			continue
		case agentproto.TypeRequest:
		default:
			continue
		}
		resp := agentproto.Envelope{Type: agentproto.TypeResponse, ID: env.ID}
		var result any = struct{}{}
		switch env.Op {
		case agentproto.OpStateApply:
			var p agentproto.StateApplyParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.applies = append(a.applies, p)
			a.mu.Unlock()
			res := agentproto.StateApplyResult{}
			for _, s := range p.Sites {
				res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
			}
			for _, d := range p.Databases {
				res.Databases = append(res.Databases, agentproto.DatabaseResult{ID: d.ID, Generation: d.Generation, OK: true})
			}
			result = res
		case agentproto.OpTransferExport:
			var p agentproto.TransferExportParams
			json.Unmarshal(env.Params, &p)
			go func() {
				for i := range transferChunks {
					<-a.credit
					a.send(agentproto.Envelope{Type: agentproto.TypeStreamData, Stream: p.Stream, Data: []byte(strings.Repeat("x", 100) + string(rune('a'+i%26)))})
				}
				res, _ := json.Marshal(agentproto.TransferResult{Bytes: transferChunks, Files: true, Databases: len(p.Databases)})
				a.send(agentproto.Envelope{Type: agentproto.TypeStreamClose, Stream: p.Stream, Reason: "done", Result: res})
			}()
		case agentproto.OpTransferImport:
			var p agentproto.TransferImportParams
			json.Unmarshal(env.Params, &p)
			importStream = p.Stream
			a.send(agentproto.Envelope{Type: agentproto.TypeStreamCredit, Stream: p.Stream, Credit: agentproto.TransferWindow})
		case agentproto.OpTransferFinish:
			var p agentproto.TransferFinishParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.finished = append(a.finished, p)
			fail := a.finish
			a.mu.Unlock()
			if fail != nil {
				resp.Error, result = fail, nil
			} else {
				result = agentproto.TransferFinishResult{Status: 200}
			}
		case agentproto.OpTransferDiscard:
			a.mu.Lock()
			a.discards++
			a.mu.Unlock()
		}
		if result != nil {
			resp.Result, _ = json.Marshal(result)
		}
		a.send(resp)
	}
}

type moveBody struct {
	ID            string  `json:"id"`
	Phase         string  `json:"phase"`
	Error         *string `json:"error"`
	Bytes         int64   `json:"bytes"`
	CheckStatus   *int    `json:"check_status"`
	TargetCleanup bool    `json:"target_cleanup"`
}

func siteSpec(p agentproto.StateApplyParams, id string) *agentproto.SiteSpec {
	for i := range p.Sites {
		if p.Sites[i].ID == id {
			return &p.Sites[i]
		}
	}
	return nil
}

func dbSpec(p agentproto.StateApplyParams, id string) *agentproto.DatabaseSpec {
	for i := range p.Databases {
		if p.Databases[i].ID == id {
			return &p.Databases[i]
		}
	}
	return nil
}

func TestMoveSite(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	src, dst := newMoveAgent(t, p, fromCfg), newMoveAgent(t, p, toCfg)

	var site struct {
		ID        string `json:"id"`
		ServerID  string `json:"server_id"`
		State     string `json:"state"`
		LinuxUser string `json:"linux_user"`
	}
	if st := c.do("POST", "/api/v1/sites", map[string]any{"domain": "shop.example.com", "server_id": fromID, "kind": "static"}, &site); st != http.StatusCreated {
		t.Fatalf("create site: %d", st)
	}
	var created struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	if st := c.do("POST", "/api/v1/databases", map[string]any{"server_id": fromID, "engine": "mariadb", "name": "shop"}, &created); st != http.StatusCreated {
		t.Fatalf("create database: %d", st)
	}
	db := created.Database
	waitUntil(t, "site and database active", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
		var d struct {
			State string `json:"state"`
		}
		c.do("GET", "/api/v1/databases/"+db.ID, nil, &d)
		return site.State == "active" && d.State == "active"
	})

	var e errBody
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": fromID}, &e); st != http.StatusBadRequest || e.Code != "same_server" {
		t.Fatalf("move to the same server: %d %s", st, e.Code)
	}
	var m moveBody
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": toID, "database_ids": []string{db.ID}}, &m); st != http.StatusAccepted {
		t.Fatalf("move: %d", st)
	}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": toID}, &e); st != http.StatusConflict || e.Code != "move_open" {
		t.Fatalf("second move: %d %s", st, e.Code)
	}
	var moves []moveBody
	waitUntil(t, "the move", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &moves)
		return len(moves) == 1 && (moves[0].Phase == "moved" || moves[0].Phase == "failed")
	})
	if moves[0].Phase != "moved" || moves[0].Bytes != transferChunks || moves[0].CheckStatus == nil || *moves[0].CheckStatus != 200 {
		t.Fatalf("move: %+v (error %v)", moves[0], moves[0].Error)
	}
	dst.mu.Lock()
	if dst.received != transferChunks || len(dst.finished) != 1 || dst.finished[0].User != site.LinuxUser ||
		len(dst.finished[0].Databases) != 1 || dst.finished[0].Databases[0].Name != "shop" {
		t.Fatalf("target got %d chunks, finish %+v", dst.received, dst.finished)
	}
	dst.mu.Unlock()
	c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
	if site.ServerID != toID {
		t.Fatalf("site is on %s, want the new server", site.ServerID)
	}
	// The target has the site and the database; the source still serves its old copy.
	if s := siteSpec(dst.lastApply(), site.ID); s == nil || s.Deleted {
		t.Fatalf("target spec %+v", s)
	}
	if d := dbSpec(dst.lastApply(), db.ID); d == nil || d.Deleted {
		t.Fatalf("target database %+v", d)
	}
	if s := siteSpec(src.lastApply(), site.ID); s == nil || s.Deleted || s.User != site.LinuxUser {
		t.Fatalf("source should keep the old copy: %+v", s)
	}
	// The site cannot be deleted while its old copy is still out there.
	if st := c.do("DELETE", "/api/v1/sites/"+site.ID, nil, &e); st != http.StatusConflict || e.Code != "move_open" {
		t.Fatalf("delete during a move: %d %s", st, e.Code)
	}

	// Removing the old copy deletes it on the source, and only there.
	if st := c.do("POST", "/api/v1/moves/"+moves[0].ID+"/cleanup", nil, nil); st != http.StatusAccepted {
		t.Fatalf("cleanup: %d", st)
	}
	waitUntil(t, "the old copy removed", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &moves)
		return moves[0].Phase == "done"
	})
	sp := src.lastApply()
	if s := siteSpec(sp, site.ID); s == nil || !s.Deleted {
		t.Fatalf("source should delete the old copy: %+v", s)
	}
	if d := dbSpec(sp, db.ID); d == nil || !d.Deleted || d.Name != "shop" {
		t.Fatalf("source should drop the old database: %+v", d)
	}
	var d struct {
		ServerID string `json:"server_id"`
		State    string `json:"state"`
	}
	c.do("GET", "/api/v1/databases/"+db.ID, nil, &d)
	if d.ServerID != toID || d.State != "active" {
		t.Fatalf("database after cleanup: %+v", d)
	}
	// Later applies on the source no longer mention the site.
	p.runner.Reconcile(context.Background(), fromID)
	if s := siteSpec(src.lastApply(), site.ID); s != nil {
		t.Fatalf("source still has the site after the move finished: %+v", s)
	}
}

func TestMoveSiteRollsBack(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	newMoveAgent(t, p, fromCfg)
	dst := newMoveAgent(t, p, toCfg)
	dst.finish = &agentproto.OpError{Code: agentproto.ErrFailed, Message: "disk full"}

	var site struct {
		ID       string `json:"id"`
		ServerID string `json:"server_id"`
		State    string `json:"state"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"domain": "shop.example.com", "server_id": fromID, "kind": "static"}, &site)
	waitUntil(t, "site active", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
		return site.State == "active"
	})
	var m moveBody
	c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": toID}, &m)
	var moves []moveBody
	waitUntil(t, "the move to fail", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &moves)
		return len(moves) == 1 && moves[0].Phase == "failed"
	})
	if moves[0].Error == nil || !strings.Contains(*moves[0].Error, "disk full") || moves[0].TargetCleanup {
		t.Fatalf("failed move: %+v", moves[0])
	}
	c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
	if site.ServerID != fromID {
		t.Fatalf("site is on %s after a failed move, want it back on the source", site.ServerID)
	}
	// The target removed its half-made copy.
	if s := siteSpec(dst.lastApply(), site.ID); s == nil || !s.Deleted {
		t.Fatalf("target should delete its half-made copy: %+v", s)
	}
	// A new move can start.
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": toID}, nil); st != http.StatusAccepted {
		t.Fatalf("move again: %d", st)
	}
}
