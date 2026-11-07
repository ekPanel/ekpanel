package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

func caddyJSON(ts time.Time, uri string, status int, size int64) string {
	return fmt.Sprintf(`{"level":"info","ts":%d.25,"logger":"http.log.access","msg":"handled request","request":{"remote_ip":"10.0.2.2","client_ip":"203.0.113.7","proto":"HTTP/2.0","method":"GET","host":"shop.example.com","uri":%q,"headers":{"User-Agent":["curl/8"],"Cookie":["REDACTED"]}},"duration":0.012,"size":%d,"status":%d}`+"\n",
		ts.Unix(), uri, size, status)
}

func writeLog(t *testing.T, f *fakeSystem, name, content string, appendTo bool) string {
	t.Helper()
	p := filepath.Join(f.root, caddyLogDir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	flags := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendTo {
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	fh, err := os.OpenFile(p, flags, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(content)
	fh.Close()
	return p
}

func TestAccessLog(t *testing.T) {
	f := newFakeSystem(t)
	now := time.Now().UTC()
	var lines strings.Builder
	for i := range 5 {
		lines.WriteString(caddyJSON(now, fmt.Sprintf("/p%d?x=1", i), 200+i*100%400, 100))
	}
	writeLog(t, f, "site-"+siteID+".log", lines.String()+`{"partial":`, false)
	res, opErr := accessLog(f.system, agentproto.AccessLogParams{Kind: "site", ID: siteID, Lines: 3})
	if opErr != nil {
		t.Fatal(opErr)
	}
	if len(res.Entries) != 3 || res.Entries[2].URI != "/p4?x=1" || res.Entries[0].IP != "203.0.113.7" || res.Entries[0].UserAgent != "curl/8" {
		t.Fatalf("entries %+v", res.Entries)
	}
	// Following: only lines after the cursor, and the partial line once it is complete.
	writeLog(t, f, "site-"+siteID+".log", `"x"}`+"\n"+caddyJSON(now, "/new", 404, 10), true)
	res, _ = accessLog(f.system, agentproto.AccessLogParams{Kind: "site", ID: siteID, Cursor: res.Cursor})
	if len(res.Entries) != 1 || res.Entries[0].URI != "/new" || res.Entries[0].Status != 404 {
		t.Fatalf("follow %+v", res.Entries)
	}
	if _, opErr := accessLog(f.system, agentproto.AccessLogParams{Kind: "site", ID: "../../etc/shadow"}); opErr == nil {
		t.Fatal("path accepted")
	}
	if res, opErr := accessLog(f.system, agentproto.AccessLogParams{Kind: "app", ID: appID}); opErr != nil || len(res.Entries) != 0 {
		t.Fatal("a missing log is empty, not an error")
	}
}

func TestTrafficCollect(t *testing.T) {
	f := newFakeSystem(t)
	hour := time.Now().UTC().Truncate(time.Hour)
	name := "site-" + siteID + ".log"
	writeLog(t, f, name, caddyJSON(hour.Add(time.Minute), "/", 200, 1000)+caddyJSON(hour.Add(2*time.Minute), "/a?q", 404, 10), false)
	p := agentproto.TrafficParams{Sites: []string{siteID}, Since: hour.Add(-2 * time.Hour)}
	res, _ := collectTraffic(f.system, p)
	if len(res.Buckets) != 1 || res.Buckets[0].Requests != 2 || res.Buckets[0].Bytes != 1010 || res.Buckets[0].Status != [4]int64{1, 0, 1, 0} {
		t.Fatalf("buckets %+v", res.Buckets)
	}
	// Asking again counts nothing twice.
	res, _ = collectTraffic(f.system, p)
	if res.Buckets[0].Requests != 2 {
		t.Fatalf("counted twice: %+v", res.Buckets)
	}
	// Roll over: the old file is renamed with one more line, a new file starts.
	full := filepath.Join(f.root, caddyLogDir, name)
	writeLog(t, f, name, caddyJSON(hour.Add(3*time.Minute), "/a", 200, 5), true)
	os.Rename(full, filepath.Join(f.root, caddyLogDir, "site-"+siteID+"-2026-11-07T10-00-00.000.log"))
	writeLog(t, f, name, caddyJSON(hour.Add(4*time.Minute), "/a", 500, 5), false)
	res, _ = collectTraffic(f.system, p)
	b := res.Buckets[0]
	if b.Requests != 4 || b.Status[3] != 1 || b.Paths["/a"] != 3 || b.Paths["/"] != 1 {
		t.Fatalf("after roll over %+v", b)
	}
	// A site no longer asked for is forgotten.
	res, _ = collectTraffic(f.system, agentproto.TrafficParams{Since: p.Since})
	if len(res.Buckets) != 0 {
		t.Fatal("stale buckets kept")
	}
}
