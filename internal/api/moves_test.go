package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"ekpanel/internal/agent"
	"ekpanel/internal/agentproto"
)

// moveAgent is a fake agent that applies state, and exports or imports a transfer of a
// fixed size with flow control, like a real one.
type moveAgent struct {
	t         *testing.T
	conn      *websocket.Conn
	mu        sync.Mutex
	applies   []agentproto.StateApplyParams
	received  int
	credit    chan struct{}
	finish    *agentproto.OpError // what OpTransferFinish answers
	finished  []agentproto.TransferFinishParams
	exports   []agentproto.TransferExportParams
	discards  int
	snapshots []agentproto.BackupSnapshot
	restores  []agentproto.BackupRestoreParams
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
			for _, ap := range p.Apps {
				res.Apps = append(res.Apps, agentproto.AppResult{ID: ap.ID, Generation: ap.Generation, OK: true})
			}
			result = res
		case agentproto.OpTransferExport:
			var p agentproto.TransferExportParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.exports = append(a.exports, p)
			a.mu.Unlock()
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
		case agentproto.OpBackupSnapshots:
			a.mu.Lock()
			result = agentproto.BackupSnapshotsResult{Snapshots: a.snapshots}
			a.mu.Unlock()
		case agentproto.OpBackupRestore:
			var p agentproto.BackupRestoreParams
			json.Unmarshal(env.Params, &p)
			a.mu.Lock()
			a.restores = append(a.restores, p)
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

func TestMoveRecoveredAfterRestart(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	newMoveAgent(t, p, fromCfg)
	dst := newMoveAgent(t, p, toCfg)
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
	// A move that the panel's restart cut off in the middle of the switch.
	ctx := context.Background()
	var moveID string
	if err := p.pool.QueryRow(ctx, `INSERT INTO moves (site_id, from_server, to_server, linux_user, phase)
		SELECT id, server_id, $2, linux_user, 'copying' FROM sites WHERE id = $1 RETURNING id`, site.ID, toID).Scan(&moveID); err != nil {
		t.Fatal(err)
	}
	m, err := p.store.GetMove(ctx, moveID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.store.SwitchMove(ctx, m); err != nil {
		t.Fatal(err)
	}
	p.runner.RecoverMoves(ctx)

	var moves []moveBody
	c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &moves)
	if moves[0].Phase != "failed" || moves[0].Error == nil || !strings.Contains(*moves[0].Error, "restarted") {
		t.Fatalf("move after a restart: %+v", moves[0])
	}
	c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
	if site.ServerID != fromID {
		t.Fatalf("site is on %s, want it back on the source", site.ServerID)
	}
	if s := siteSpec(dst.lastApply(), site.ID); s == nil || !s.Deleted {
		t.Fatalf("target should delete its half-made copy: %+v", s)
	}
	// The website can be moved (or deleted) again.
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/move", map[string]any{"server_id": toID}, nil); st != http.StatusAccepted {
		t.Fatalf("move again: %d", st)
	}
}

func appSpec(p agentproto.StateApplyParams, id string) *agentproto.AppSpec {
	for i := range p.Apps {
		if p.Apps[i].ID == id {
			return &p.Apps[i]
		}
	}
	return nil
}

func TestMoveApp(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	src, dst := newMoveAgent(t, p, fromCfg), newMoveAgent(t, p, toCfg)

	type appBody struct {
		ID       string `json:"id"`
		ServerID string `json:"server_id"`
		State    string `json:"state"`
		HostPort *int   `json:"host_port"`
	}
	port := 80
	var other, app appBody
	// The target already uses the first app port, so the moved app must get another.
	c.do("POST", "/api/v1/apps", map[string]any{"server_id": toID, "name": "blog", "kind": "image", "image": "nginx:1", "container_port": port}, &other)
	if st := c.do("POST", "/api/v1/apps", map[string]any{"server_id": fromID, "name": "shop", "kind": "image", "image": "nginx:1",
		"container_port": port, "volumes": []string{"/data"}}, &app); st != http.StatusCreated {
		t.Fatalf("create app: %d", st)
	}
	waitUntil(t, "apps active", func() bool {
		c.do("GET", "/api/v1/apps/"+app.ID, nil, &app)
		c.do("GET", "/api/v1/apps/"+other.ID, nil, &other)
		return app.State == "active" && other.State == "active"
	})
	if *app.HostPort != *other.HostPort {
		t.Fatalf("test setup: both apps should start on the same port number (%d, %d)", *app.HostPort, *other.HostPort)
	}
	var m moveBody
	if st := c.do("POST", "/api/v1/apps/"+app.ID+"/move", map[string]any{"server_id": toID}, &m); st != http.StatusAccepted {
		t.Fatalf("move app: %d", st)
	}
	var moves []moveBody
	waitUntil(t, "the app move", func() bool {
		c.do("GET", "/api/v1/apps/"+app.ID+"/moves", nil, &moves)
		return len(moves) == 1 && (moves[0].Phase == "moved" || moves[0].Phase == "failed")
	})
	if moves[0].Phase != "moved" {
		t.Fatalf("app move: %+v (error %v)", moves[0], moves[0].Error)
	}
	c.do("GET", "/api/v1/apps/"+app.ID, nil, &app)
	if app.ServerID != toID || *app.HostPort == *other.HostPort {
		t.Fatalf("app after the move: server %s, port %d (the other app has %d)", app.ServerID, *app.HostPort, *other.HostPort)
	}
	src.mu.Lock()
	if len(src.exports) != 1 || src.exports[0].App != "shop" {
		t.Fatalf("export %+v", src.exports)
	}
	src.mu.Unlock()
	dst.mu.Lock()
	if len(dst.finished) != 1 || dst.finished[0].App != "shop" {
		t.Fatalf("finish %+v", dst.finished)
	}
	dst.mu.Unlock()
	if a := appSpec(dst.lastApply(), app.ID); a == nil || a.Stopped || a.Deleted {
		t.Fatalf("target app spec %+v", a)
	}
	if a := appSpec(src.lastApply(), app.ID); a == nil || !a.Stopped || a.Deleted {
		t.Fatalf("source should keep the old copy stopped: %+v", a)
	}
	var e errBody
	if st := c.do("DELETE", "/api/v1/apps/"+app.ID, nil, &e); st != http.StatusConflict || e.Code != "move_open" {
		t.Fatalf("delete during a move: %d %s", st, e.Code)
	}
	c.do("POST", "/api/v1/moves/"+moves[0].ID+"/cleanup", nil, nil)
	waitUntil(t, "the old copy removed", func() bool {
		c.do("GET", "/api/v1/apps/"+app.ID+"/moves", nil, &moves)
		return moves[0].Phase == "done"
	})
	if a := appSpec(src.lastApply(), app.ID); a == nil || !a.Deleted || !a.DeleteData {
		t.Fatalf("source should delete the old copy with its data: %+v", a)
	}
	// A name the target already uses cannot move there.
	var dup appBody
	c.do("POST", "/api/v1/apps", map[string]any{"server_id": fromID, "name": "blog", "kind": "image", "image": "nginx:1"}, &dup)
	waitUntil(t, "app active", func() bool {
		c.do("GET", "/api/v1/apps/"+dup.ID, nil, &dup)
		return dup.State == "active"
	})
	if st := c.do("POST", "/api/v1/apps/"+dup.ID+"/move", map[string]any{"server_id": toID}, &e); st != http.StatusConflict || e.Code != "app_name_taken" {
		t.Fatalf("move onto a taken name: %d %s", st, e.Code)
	}
}

func TestMoveAppStaysStoppedWhileCopying(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, _, _, _ := enrolled(t, p, c, "new")
	src := newMoveAgent(t, p, fromCfg)
	var app struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	c.do("POST", "/api/v1/apps", map[string]any{"server_id": fromID, "name": "shop", "kind": "image", "image": "nginx:1"}, &app)
	waitUntil(t, "app active", func() bool {
		c.do("GET", "/api/v1/apps/"+app.ID, nil, &app)
		return app.State == "active"
	})
	ctx := context.Background()
	var moveID string
	if err := p.pool.QueryRow(ctx, `INSERT INTO moves (app_id, from_server, to_server, phase) VALUES ($1, $2, $3, 'copying') RETURNING id`,
		app.ID, fromID, toID).Scan(&moveID); err != nil {
		t.Fatal(err)
	}
	// Any apply of the source while the copy runs (the drift check, another change) keeps it stopped.
	p.runner.Reconcile(ctx, fromID)
	if a := appSpec(src.lastApply(), app.ID); a == nil || !a.Stopped || a.Deleted {
		t.Fatalf("app during the copy: %+v", a)
	}
	p.pool.Exec(ctx, `UPDATE moves SET phase = 'failed' WHERE id = $1`, moveID)
	p.runner.Reconcile(ctx, fromID)
	if a := appSpec(src.lastApply(), app.ID); a == nil || a.Stopped {
		t.Fatalf("app after a failed copy: %+v", a)
	}
}

func TestMoveDatabase(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	src, dst := newMoveAgent(t, p, fromCfg), newMoveAgent(t, p, toCfg)
	var created struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	c.do("POST", "/api/v1/databases", map[string]any{"server_id": fromID, "engine": "postgresql", "name": "reports"}, &created)
	db := created.Database
	waitUntil(t, "database active", func() bool {
		var d struct {
			State string `json:"state"`
		}
		c.do("GET", "/api/v1/databases/"+db.ID, nil, &d)
		return d.State == "active"
	})
	if st := c.do("POST", "/api/v1/databases/"+db.ID+"/move", map[string]any{"server_id": toID}, nil); st != http.StatusAccepted {
		t.Fatalf("move database: %d", st)
	}
	var moves []moveBody
	waitUntil(t, "the database move", func() bool {
		c.do("GET", "/api/v1/databases/"+db.ID+"/moves", nil, &moves)
		return len(moves) == 1 && (moves[0].Phase == "moved" || moves[0].Phase == "failed")
	})
	if moves[0].Phase != "moved" {
		t.Fatalf("database move: %+v (error %v)", moves[0], moves[0].Error)
	}
	src.mu.Lock()
	if len(src.exports) != 1 || len(src.exports[0].Databases) != 1 || src.exports[0].User != "" || src.exports[0].App != "" {
		t.Fatalf("export %+v", src.exports)
	}
	src.mu.Unlock()
	dst.mu.Lock()
	if len(dst.finished) != 1 || len(dst.finished[0].Databases) != 1 || dst.finished[0].Databases[0].Name != "reports" {
		t.Fatalf("finish %+v", dst.finished)
	}
	dst.mu.Unlock()
	var e errBody
	if st := c.do("DELETE", "/api/v1/databases/"+db.ID, nil, &e); st != http.StatusConflict || e.Code != "move_open" {
		t.Fatalf("delete during a move: %d %s", st, e.Code)
	}
	// A website of the same owner stayed on the old server: it may use the old copy.
	c.do("POST", "/api/v1/sites", map[string]any{"domain": "stays.example.com", "server_id": fromID, "kind": "static"}, nil)
	if st := c.do("POST", "/api/v1/moves/"+moves[0].ID+"/cleanup", nil, &e); st != http.StatusConflict || e.Code != "old_copy_in_use" {
		t.Fatalf("cleanup while a website stays: %d %s", st, e.Code)
	}
	if st := c.do("POST", "/api/v1/moves/"+moves[0].ID+"/cleanup?force=1", nil, nil); st != http.StatusAccepted {
		t.Fatalf("forced cleanup: %d", st)
	}
	waitUntil(t, "the old copy removed", func() bool {
		c.do("GET", "/api/v1/databases/"+db.ID+"/moves", nil, &moves)
		return moves[0].Phase == "done"
	})
	if d := dbSpec(src.lastApply(), db.ID); d == nil || !d.Deleted {
		t.Fatalf("source should drop the old copy: %+v", d)
	}
	if d := dbSpec(dst.lastApply(), db.ID); d == nil || d.Deleted {
		t.Fatalf("target database %+v", d)
	}
}

func TestMoveCustomer(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "old")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	newMoveAgent(t, p, fromCfg)
	dst := newMoveAgent(t, p, toCfg)
	var pkg struct {
		ID string `json:"id"`
	}
	if st := c.do("POST", "/api/v1/packages", map[string]any{"name": "Pro", "max_sites": 5, "max_databases": 5, "max_apps": 5,
		"disk_mb": 1000, "php_versions": []string{"8.4"}, "app_images": []string{"nginx"}, "server_ids": []string{fromID, toID}}, &pkg); st != http.StatusCreated {
		t.Fatalf("package: %d", st)
	}
	var cust struct {
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
	}
	c.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &cust)
	owner := cust.Customer.ID
	var site, app struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	var db struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"domain": "shop.example.com", "server_id": fromID, "kind": "static", "owner_id": owner}, &site)
	c.do("POST", "/api/v1/databases", map[string]any{"server_id": fromID, "engine": "mariadb", "name": "shopdb", "owner_id": owner}, &db)
	c.do("POST", "/api/v1/apps", map[string]any{"server_id": fromID, "name": "web", "kind": "image", "image": "nginx:1", "owner_id": owner}, &app)
	waitUntil(t, "everything active", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
		c.do("GET", "/api/v1/apps/"+app.ID, nil, &app)
		return site.State == "active" && app.State == "active"
	})
	var e errBody
	if st := c.do("POST", "/api/v1/customers/"+owner+"/move", map[string]any{"from_server_id": fromID, "server_id": fromID}, &e); st != http.StatusBadRequest {
		t.Fatalf("same servers: %d", st)
	}
	if st := c.do("POST", "/api/v1/customers/"+owner+"/move", map[string]any{"from_server_id": fromID, "server_id": toID}, nil); st != http.StatusAccepted {
		t.Fatalf("move customer: %d", st)
	}
	var siteMoves, appMoves []moveBody
	waitUntil(t, "the customer's things to move", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &siteMoves)
		c.do("GET", "/api/v1/apps/"+app.ID+"/moves", nil, &appMoves)
		return len(siteMoves) == 1 && siteMoves[0].Phase == "moved" && len(appMoves) == 1 && appMoves[0].Phase == "moved"
	})
	// The website took the customer's database along.
	dst.mu.Lock()
	var tookDB bool
	for _, f := range dst.finished {
		if f.User != "" && len(f.Databases) == 1 && f.Databases[0].Name == "shopdb" {
			tookDB = true
		}
	}
	dst.mu.Unlock()
	if !tookDB {
		t.Fatalf("the website's move did not take the database: %+v", dst.finished)
	}
	var d struct {
		ServerID string `json:"server_id"`
	}
	c.do("GET", "/api/v1/databases/"+db.Database.ID, nil, &d)
	if d.ServerID != toID {
		t.Fatalf("database on %s after the customer move", d.ServerID)
	}
	if st := c.do("POST", "/api/v1/customers/"+owner+"/move", map[string]any{"from_server_id": fromID, "server_id": toID}, &e); st != http.StatusConflict && st != http.StatusBadRequest {
		t.Fatalf("second customer move: %d %s", st, e.Code)
	}
}

func TestRebuildSiteFromBackup(t *testing.T) {
	p := newTestPanel(t)
	c := newClient(t, p.web.URL)
	setUp(t, c)
	fromID, fromCfg, _, _ := enrolled(t, p, c, "lost")
	toID, toCfg, _, _ := enrolled(t, p, c, "new")
	src := newMoveAgent(t, p, fromCfg)
	dst := newMoveAgent(t, p, toCfg)
	var site struct {
		ID        string `json:"id"`
		ServerID  string `json:"server_id"`
		State     string `json:"state"`
		LinuxUser string `json:"linux_user"`
	}
	c.do("POST", "/api/v1/sites", map[string]any{"domain": "shop.example.com", "server_id": fromID, "kind": "static"}, &site)
	var created struct {
		Database struct {
			ID string `json:"id"`
		} `json:"database"`
	}
	c.do("POST", "/api/v1/databases", map[string]any{"server_id": fromID, "engine": "mariadb", "name": "shop"}, &created)
	db := created.Database
	waitUntil(t, "site and database active", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
		var d struct {
			State string `json:"state"`
		}
		c.do("GET", "/api/v1/databases/"+db.ID, nil, &d)
		return site.State == "active" && d.State == "active"
	})
	// The old server is gone.
	src.conn.CloseNow()
	ctx := context.Background()
	var repoID string
	if err := p.pool.QueryRow(ctx, `INSERT INTO backup_repos (server_id, name, kind, location) VALUES ($1, 'S3', 's3', 's3:bucket') RETURNING id`,
		toID).Scan(&repoID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	dst.snapshots = []agentproto.BackupSnapshot{
		{ID: "aaaa1111", Time: now.Add(-48 * time.Hour), Kind: "site", ItemID: site.ID},
		{ID: "bbbb2222", Time: now.Add(-time.Hour), Kind: "site", ItemID: site.ID},
	}
	var e errBody
	body := map[string]any{"server_id": toID, "repo_id": repoID, "database_ids": []string{db.ID}}
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/rebuild", body, &e); st != http.StatusConflict || e.Code != "no_snapshot" {
		t.Fatalf("rebuild without a database backup: %d %s", st, e.Code)
	}
	dst.mu.Lock()
	dst.snapshots = append(dst.snapshots, agentproto.BackupSnapshot{ID: "cccc3333", Time: now.Add(-2 * time.Hour), Kind: "database", ItemID: db.ID})
	dst.mu.Unlock()
	if st := c.do("POST", "/api/v1/sites/"+site.ID+"/rebuild", body, nil); st != http.StatusAccepted {
		t.Fatalf("rebuild: %d", st)
	}
	var moves []struct {
		moveBody
		FromBackup *string `json:"from_backup"`
	}
	waitUntil(t, "the rebuild", func() bool {
		c.do("GET", "/api/v1/sites/"+site.ID+"/moves", nil, &moves)
		return len(moves) == 1 && (moves[0].Phase == "moved" || moves[0].Phase == "failed")
	})
	if moves[0].Phase != "moved" || moves[0].FromBackup == nil || *moves[0].FromBackup != repoID {
		t.Fatalf("rebuild: %+v (error %v)", moves[0], moves[0].Error)
	}
	c.do("GET", "/api/v1/sites/"+site.ID, nil, &site)
	if site.ServerID != toID {
		t.Fatalf("site on %s after the rebuild", site.ServerID)
	}
	dst.mu.Lock()
	defer dst.mu.Unlock()
	if len(dst.restores) != 2 ||
		dst.restores[0].Site == nil || dst.restores[0].SnapshotID != "bbbb2222" || dst.restores[0].Mode != "replace" || dst.restores[0].Site.User != site.LinuxUser ||
		dst.restores[1].Database == nil || dst.restores[1].SnapshotID != "cccc3333" || dst.restores[1].RepoID != repoID {
		t.Fatalf("restores: %+v", dst.restores)
	}
}
