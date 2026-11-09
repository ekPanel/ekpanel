package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func TestTrafficAndLogs(t *testing.T) {
	p := newTestPanel(t)
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	serverID, cfg, _, _ := enrolled(t, p, admin, "web-1")
	tlsCfg, _ := cfg.TLSConfig()
	conn, _, err := dialAgent(t, p, tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	var siteID string
	hour := time.Now().UTC().Truncate(time.Hour)
	fakeAgent(t, conn, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		switch op {
		case agentproto.OpTraffic:
			var in agentproto.TrafficParams
			json.Unmarshal(raw, &in)
			var res agentproto.TrafficResult
			for _, id := range in.Sites {
				res.Buckets = append(res.Buckets,
					agentproto.TrafficBucket{Kind: "site", ID: id, Hour: hour.Add(-time.Hour), Requests: 10, Bytes: 5000, Status: [4]int64{8, 1, 1, 0}, Paths: map[string]int64{"/": 6, "/shop": 4}},
					agentproto.TrafficBucket{Kind: "site", ID: id, Hour: hour, Requests: 5, Bytes: 1000, Status: [4]int64{4, 0, 0, 1}, Paths: map[string]int64{"/shop": 5}})
			}
			return res, nil
		case agentproto.OpAccessLog:
			var in agentproto.AccessLogParams
			json.Unmarshal(raw, &in)
			return agentproto.AccessLogResult{Entries: []agentproto.AccessLogEntry{{URI: "/" + in.Kind + "/" + in.ID, Status: 200}}, Cursor: 42}, nil
		case agentproto.OpStateApply:
			return agentproto.StateApplyResult{}, nil
		}
		return nil, &agentproto.OpError{Code: agentproto.ErrUnknownOp}
	})
	var pkg struct{ ID string }
	admin.do("POST", "/api/v1/packages", map[string]any{"name": "T", "max_sites": 1}, &pkg)
	var a, b struct {
		Customer struct{ ID string } `json:"customer"`
		Password string              `json:"password"`
	}
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "a@example.com", "name": "A", "package_id": pkg.ID}, &a)
	admin.do("POST", "/api/v1/customers", map[string]string{"email": "b@example.com", "name": "B", "package_id": pkg.ID}, &b)
	ca := login(t, p.web.URL, "a@example.com", a.Password)
	cb := login(t, p.web.URL, "b@example.com", b.Password)
	var site struct{ ID string }
	ca.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "a.example.com", "kind": "static"}, &site)
	siteID = site.ID

	// Collecting twice stores the same totals: hours are replaced, not added.
	for range 2 {
		if st := admin.do("POST", "/api/v1/servers/"+serverID+"/traffic", nil, nil); st != http.StatusNoContent {
			t.Fatalf("collect: %d", st)
		}
	}
	var tr struct {
		Points   []struct{ Requests int64 } `json:"points"`
		Requests int64                      `json:"requests"`
		Bytes    int64                      `json:"bytes"`
		Status   [4]int64                   `json:"status"`
		Paths    []struct {
			Path     string `json:"path"`
			Requests int64  `json:"requests"`
		} `json:"paths"`
	}
	ca.do("GET", "/api/v1/sites/"+siteID+"/traffic?range=24h", nil, &tr)
	if tr.Requests != 15 || tr.Bytes != 6000 || len(tr.Points) != 2 || tr.Status != [4]int64{12, 1, 1, 1} || tr.Paths[0].Path != "/shop" || tr.Paths[0].Requests != 9 {
		t.Fatalf("traffic %+v", tr)
	}
	ca.do("GET", "/api/v1/sites/"+siteID+"/traffic?range=7d", nil, &tr)
	if len(tr.Points) != 1 || tr.Requests != 15 {
		t.Errorf("daily %+v", tr)
	}
	var usage struct {
		BandwidthMonth int64 `json:"bandwidth_month"`
	}
	ca.do("GET", "/api/v1/me/usage", nil, &usage)
	if usage.BandwidthMonth != 6000 && hour.Day() != 1 {
		t.Errorf("bandwidth %d", usage.BandwidthMonth)
	}
	var logs agentproto.AccessLogResult
	ca.do("GET", "/api/v1/sites/"+siteID+"/logs?kind=access", nil, &logs)
	if len(logs.Entries) != 1 || logs.Entries[0].URI != "/site/"+siteID || logs.Cursor != 42 {
		t.Fatalf("logs %+v", logs)
	}
	for _, path := range []string{"/traffic", "/logs?kind=access", "/logs?kind=php"} {
		if st := cb.do("GET", "/api/v1/sites/"+siteID+path, nil, nil); st != http.StatusNotFound {
			t.Errorf("customer B %s: %d", path, st)
		}
	}
}
