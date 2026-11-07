package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"ekpanel/internal/agentproto"
)

// Caddy writes one JSON object per request to /var/log/caddy/site-<id>.log or app-<id>.log
// and rolls the file at 20 MB into site-<id>-<time>.log.
const caddyLogDir = "/var/log/caddy"

func accessLogPath(kind, id string) (string, bool) {
	if (kind != "site" && kind != "app") || !uuidRE.MatchString(id) {
		return "", false
	}
	return caddyLogDir + "/" + kind + "-" + id + ".log", true
}

type caddyLine struct {
	TS      float64 `json:"ts"`
	Request struct {
		ClientIP string              `json:"client_ip"`
		RemoteIP string              `json:"remote_ip"`
		Method   string              `json:"method"`
		Host     string              `json:"host"`
		URI      string              `json:"uri"`
		Proto    string              `json:"proto"`
		Headers  map[string][]string `json:"headers"`
	} `json:"request"`
	Status   int     `json:"status"`
	Size     int64   `json:"size"`
	Duration float64 `json:"duration"`
}

func parseCaddyLine(b []byte) (caddyLine, bool) {
	var l caddyLine
	if json.Unmarshal(b, &l) != nil || l.Status == 0 {
		return l, false
	}
	return l, true
}

func (l caddyLine) entry() agentproto.AccessLogEntry {
	ip := l.Request.ClientIP
	if ip == "" {
		ip = l.Request.RemoteIP
	}
	header := func(k string) string {
		if v := l.Request.Headers[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	sec := int64(l.TS)
	return agentproto.AccessLogEntry{
		Time: time.Unix(sec, int64((l.TS-float64(sec))*1e9)).UTC(), IP: ip, Method: l.Request.Method, Host: l.Request.Host,
		URI: l.Request.URI, Proto: l.Request.Proto, Status: l.Status, Size: l.Size, DurationMS: l.Duration * 1000,
		UserAgent: header("User-Agent"), Referer: header("Referer"),
	}
}

const maxLogRead = 1 << 20

func accessLog(sys *system, p agentproto.AccessLogParams) (*agentproto.AccessLogResult, *agentproto.OpError) {
	path, ok := accessLogPath(p.Kind, p.ID)
	if !ok {
		return nil, opError(agentproto.ErrInvalidParams, "invalid log")
	}
	lines := p.Lines
	if lines <= 0 || lines > 1000 {
		lines = 200
	}
	res := &agentproto.AccessLogResult{Entries: []agentproto.AccessLogEntry{}}
	f, err := os.Open(sys.path(path))
	if os.IsNotExist(err) {
		return res, nil // no requests yet
	}
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	size := info.Size()
	start := p.Cursor
	if start <= 0 || start > size {
		// The newest lines; also after the log was rolled over (it got smaller).
		start = max(0, size-256<<10)
	}
	if start < size-maxLogRead {
		start = size - maxLogRead
	}
	data := make([]byte, size-start)
	if _, err := f.ReadAt(data, start); err != nil && err != io.EOF {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	if start > 0 && start != p.Cursor {
		// Started inside a line: skip to the next one.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data, start = data[i+1:], start+int64(i+1)
		}
	}
	// Only whole lines; a line being written stays for the next read.
	end := bytes.LastIndexByte(data, '\n') + 1
	res.Cursor = start + int64(end)
	for line := range bytes.Lines(data[:end]) {
		if l, ok := parseCaddyLine(line); ok {
			res.Entries = append(res.Entries, l.entry())
		}
	}
	if len(res.Entries) > lines {
		res.Entries = res.Entries[len(res.Entries)-lines:]
	}
	return res, nil
}

// ---- Traffic ----

// trafficMu keeps two collections from reading the same lines twice.
var trafficMu sync.Mutex

const (
	trafficStatePath = "/var/lib/ekpanel-agent/traffic.json"
	trafficKeepHours = 72
	maxPathsPerHour  = 200
)

type trafficFile struct {
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

type trafficState struct {
	Files   map[string]trafficFile               `json:"files"`
	Buckets map[string]*agentproto.TrafficBucket `json:"buckets"` // kind:id:unixhour
}

func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// readFrom adds the lines after offset in path to the buckets and returns the new offset.
func (st *trafficState) readFrom(path, kind, id string, offset int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return offset // a partial last line is read next time
		}
		offset += int64(len(line))
		l, ok := parseCaddyLine(line)
		if !ok {
			continue
		}
		hour := time.Unix(int64(l.TS), 0).UTC().Truncate(time.Hour)
		key := kind + ":" + id + ":" + hour.Format("2006010215")
		b := st.Buckets[key]
		if b == nil {
			b = &agentproto.TrafficBucket{Kind: kind, ID: id, Hour: hour, Paths: map[string]int64{}}
			st.Buckets[key] = b
		}
		b.Requests++
		b.Bytes += l.Size
		if c := l.Status/100 - 2; c >= 0 && c < 4 {
			b.Status[c]++
		}
		p, _, _ := strings.Cut(l.Request.URI, "?")
		if len(p) > 200 {
			p = p[:200]
		}
		if _, seen := b.Paths[p]; seen || len(b.Paths) < maxPathsPerHour {
			b.Paths[p]++
		}
	}
}

func collectTraffic(sys *system, p agentproto.TrafficParams) (*agentproto.TrafficResult, *agentproto.OpError) {
	st := &trafficState{Files: map[string]trafficFile{}, Buckets: map[string]*agentproto.TrafficBucket{}}
	if b, err := os.ReadFile(sys.path(trafficStatePath)); err == nil {
		json.Unmarshal(b, st)
	}
	if st.Files == nil {
		st.Files = map[string]trafficFile{}
	}
	if st.Buckets == nil {
		st.Buckets = map[string]*agentproto.TrafficBucket{}
	}
	wanted := map[string]bool{}
	targets := func(kind string, ids []string) {
		for _, id := range ids {
			path, ok := accessLogPath(kind, id)
			if !ok {
				continue
			}
			wanted[kind+":"+id] = true
			full := sys.path(path)
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			prev, known := st.Files[path]
			ino := inodeOf(info)
			if known && prev.Inode != ino {
				// Rolled over: finish the old file, now renamed, then read the new one.
				rolled, _ := filepath.Glob(sys.path(caddyLogDir + "/" + kind + "-" + id + "-*.log"))
				for _, r := range rolled {
					if ri, err := os.Stat(r); err == nil && inodeOf(ri) == prev.Inode {
						st.readFrom(r, kind, id, prev.Offset)
					}
				}
				prev.Offset = 0
			}
			if prev.Offset > info.Size() {
				prev.Offset = 0
			}
			st.Files[path] = trafficFile{Inode: ino, Offset: st.readFrom(full, kind, id, prev.Offset)}
		}
	}
	targets("site", p.Sites)
	targets("app", p.Apps)

	cutoff := time.Now().UTC().Add(-trafficKeepHours * time.Hour)
	res := &agentproto.TrafficResult{Buckets: []agentproto.TrafficBucket{}}
	for key, b := range st.Buckets {
		if b.Hour.Before(cutoff) || !wanted[b.Kind+":"+b.ID] {
			delete(st.Buckets, key)
			continue
		}
		if !b.Hour.Before(p.Since.UTC().Truncate(time.Hour)) {
			res.Buckets = append(res.Buckets, topPaths(*b, 20))
		}
	}
	for path := range st.Files {
		kind, rest, _ := strings.Cut(strings.TrimPrefix(path, caddyLogDir+"/"), "-")
		if !wanted[kind+":"+strings.TrimSuffix(rest, ".log")] {
			delete(st.Files, path)
		}
	}
	b, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(sys.path(trafficStatePath)), 0o700); err == nil {
		os.WriteFile(sys.path(trafficStatePath), b, 0o600)
	}
	sort.Slice(res.Buckets, func(i, j int) bool { return res.Buckets[i].Hour.Before(res.Buckets[j].Hour) })
	return res, nil
}

func topPaths(b agentproto.TrafficBucket, n int) agentproto.TrafficBucket {
	type kv struct {
		p string
		n int64
	}
	all := make([]kv, 0, len(b.Paths))
	for p, c := range b.Paths {
		all = append(all, kv{p, c})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n || (all[i].n == all[j].n && all[i].p < all[j].p) })
	b.Paths = map[string]int64{}
	for i := 0; i < len(all) && i < n; i++ {
		b.Paths[all[i].p] = all[i].n
	}
	return b
}

func accessLogOp(_ context.Context, sys *system, raw []byte) (any, *agentproto.OpError) {
	var p agentproto.AccessLogParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	return accessLog(sys, p)
}

func trafficOp(_ context.Context, sys *system, raw []byte) (any, *agentproto.OpError) {
	var p agentproto.TrafficParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	trafficMu.Lock()
	defer trafficMu.Unlock()
	return collectTraffic(sys, p)
}
