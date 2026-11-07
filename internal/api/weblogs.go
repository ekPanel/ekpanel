package api

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

const phpLog = "/logs/php-error.log"

func queryInt(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return n
}

func (s *Server) accessLog(w http.ResponseWriter, r *http.Request, kind, id, serverID string) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, serverID, agentproto.OpAccessLog, agentproto.AccessLogParams{
		Kind: kind, ID: id, Cursor: queryInt(r, "cursor"), Lines: int(queryInt(r, "lines")),
	})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(raw)
}

type phpLogResult struct {
	Lines  []string `json:"lines"`
	Cursor int64    `json:"cursor"`
}

// phpErrorLog reads the newest lines of a site's PHP error log. It goes through the file
// worker, which runs as the site's account inside its folder, so a link planted in the
// site's logs folder cannot make it read anything else.
func (s *Server) phpErrorLog(w http.ResponseWriter, r *http.Request, site store.Site) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := phpLogResult{Lines: []string{}}
	list, err := s.fileCall(ctx, site, agentproto.FilesParams{Action: "list", Path: "/logs"})
	if err != nil {
		writeJSON(w, http.StatusOK, res) // no logs folder yet
		return
	}
	var size int64 = -1
	for _, e := range list.Entries {
		if e.Name == "php-error.log" && e.Type == "file" {
			size = e.Size
		}
	}
	if size < 0 {
		writeJSON(w, http.StatusOK, res)
		return
	}
	start := queryInt(r, "cursor")
	if start <= 0 || start > size {
		start = max(0, size-64<<10)
	}
	chunk, err := s.fileCall(ctx, site, agentproto.FilesParams{Action: "read_chunk", Path: phpLog, Offset: start})
	if err != nil {
		s.writeFileError(w, r, err)
		return
	}
	data := chunk.Data
	if start > 0 && start != queryInt(r, "cursor") {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data, start = data[i+1:], start+int64(i+1)
		}
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	res.Cursor = start + int64(end)
	for line := range bytes.Lines(data[:end]) {
		res.Lines = append(res.Lines, string(bytes.ToValidUTF8(bytes.TrimRight(line, "\r\n"), []byte("�"))))
	}
	if len(res.Lines) > 500 {
		res.Lines = res.Lines[len(res.Lines)-500:]
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleSiteLogs(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	switch r.URL.Query().Get("kind") {
	case "php":
		s.phpErrorLog(w, r, site)
	default:
		s.accessLog(w, r, "site", site.ID, site.ServerID)
	}
}

func (s *Server) handleAppAccessLog(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	s.accessLog(w, r, "app", app.ID, app.ServerID)
}

// trafficRange turns the UI's choice into a start and a step.
func trafficRange(r *http.Request) (time.Time, time.Duration) {
	now := time.Now().UTC()
	switch r.URL.Query().Get("range") {
	case "7d":
		return now.Truncate(24*time.Hour).AddDate(0, 0, -6), 24 * time.Hour
	case "30d":
		return now.Truncate(24*time.Hour).AddDate(0, 0, -29), 24 * time.Hour
	default:
		return now.Truncate(time.Hour).Add(-23 * time.Hour), time.Hour
	}
}

func (s *Server) writeTraffic(w http.ResponseWriter, r *http.Request, kind, id string) {
	from, step := trafficRange(r)
	t, err := s.store.TrafficOf(r.Context(), kind, id, from, step)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleSiteTraffic(w http.ResponseWriter, r *http.Request) {
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.writeTraffic(w, r, "site", site.ID)
}

func (s *Server) handleAppTraffic(w http.ResponseWriter, r *http.Request) {
	app, err := s.getApp(r, appID(r))
	if err != nil {
		s.writeAppStoreError(w, r, err)
		return
	}
	s.writeTraffic(w, r, "app", app.ID)
}

// handleCollectTraffic fetches traffic from a server now instead of at the next quarter hour.
func (s *Server) handleCollectTraffic(w http.ResponseWriter, r *http.Request) {
	id := serverID(r)
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	if err := s.opts.Tasks.CollectTraffic(r.Context(), id); err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
