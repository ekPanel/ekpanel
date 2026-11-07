package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

// ---- SFTP ----

type sftpRequest struct {
	Enabled bool `json:"enabled"`
}

// siteWithPassword returns a new SFTP password; it is shown only this once.
type siteWithPassword struct {
	Site     store.Site `json:"site"`
	Password string     `json:"password,omitempty"`
}

func (s *Server) loadSite(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	site, err := s.store.GetSite(r.Context(), siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return store.Site{}, false
	}
	if site.State == "deleting" {
		writeError(w, http.StatusConflict, "site_deleting", "this site is being deleted")
		return store.Site{}, false
	}
	return site, true
}

func (s *Server) handleSetSFTP(w http.ResponseWriter, r *http.Request) {
	var req sftpRequest
	if !readJSON(w, r, &req) {
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	var password string
	var hash *string
	clear := !req.Enabled // turning SFTP off forgets the password; turning it on makes a new one
	if req.Enabled && !site.SFTPEnabled {
		password = auth.NewDatabasePassword()
		h, err := auth.LinuxPasswordHash(password)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		hash = &h
	}
	if req.Enabled && site.SFTPEnabled {
		clear = false
	}
	site, err := s.store.SetSiteSFTP(r.Context(), site.ID, req.Enabled, hash, clear, nil)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	action := "site.sftp_disabled"
	if req.Enabled {
		action = "site.sftp_enabled"
	}
	s.audit(r, action, site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, siteWithPassword{Site: site, Password: password})
}

func (s *Server) handleResetSFTPPassword(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if !site.SFTPEnabled {
		writeError(w, http.StatusConflict, "sftp_disabled", "turn on SFTP first")
		return
	}
	password := auth.NewDatabasePassword()
	h, err := auth.LinuxPasswordHash(password)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	site, err = s.store.SetSiteSFTP(r.Context(), site.ID, true, &h, false, nil)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.sftp_password_reset", site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, siteWithPassword{Site: site, Password: password})
}

type sshKeysRequest struct {
	Keys []string `json:"keys"`
}

func (s *Server) handleSetSSHKeys(w http.ResponseWriter, r *http.Request) {
	var req sshKeysRequest
	if !readJSON(w, r, &req) {
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	keys := []string{}
	for _, k := range req.Keys {
		if k == "" {
			continue
		}
		n, err := agentproto.NormalizeSSHKey(k)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_ssh_key", err.Error())
			return
		}
		keys = append(keys, n)
	}
	if len(keys) > 20 {
		writeError(w, http.StatusBadRequest, "too_many_ssh_keys", "a site can have at most 20 SSH keys")
		return
	}
	site, err := s.store.SetSiteSFTP(r.Context(), site.ID, site.SFTPEnabled, nil, false, keys)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.ssh_keys_changed", site.Domain, map[string]any{"site_id": site.ID, "count": len(keys)})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

// ---- File manager ----

// fileCall runs one file action on the site's server, as the site's account.
func (s *Server) fileCall(ctx context.Context, site store.Site, p agentproto.FilesParams) (*agentproto.FilesResult, error) {
	p.User = site.LinuxUser
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	raw, err := s.hub.Call(ctx, site.ServerID, agentproto.OpFiles, p)
	if err != nil {
		return nil, err
	}
	var res agentproto.FilesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (s *Server) writeFileError(w http.ResponseWriter, r *http.Request, err error) {
	var opErr *agentproto.OpError
	if errors.As(err, &opErr) {
		status := http.StatusBadRequest
		switch opErr.Code {
		case "not_found":
			status = http.StatusNotFound
		case "exists", "bad_offset":
			status = http.StatusConflict
		case agentproto.ErrNotAllowed:
			status = http.StatusForbidden
		case "too_large":
			status = http.StatusRequestEntityTooLarge
		case agentproto.ErrFailed:
			status = http.StatusBadGateway
		}
		writeError(w, status, "file_"+opErr.Code, opErr.Message)
		return
	}
	s.writeAgentError(w, r, err)
}

// fileActions are what the JSON endpoint accepts; chunked transfers have their own endpoints.
var fileActions = map[string]bool{"list": true, "read": true, "write": true, "mkdir": true, "rename": true,
	"delete": true, "empty_trash": true, "chmod": true, "extract": true, "compress": true}

// mutating file actions are written to the audit log.
var mutatingFileActions = map[string]bool{"write": true, "mkdir": true, "rename": true, "delete": true,
	"empty_trash": true, "chmod": true, "extract": true, "compress": true}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	var p agentproto.FilesParams
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)) // editor files up to 2 MB, as base64
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid JSON body")
		return
	}
	if !fileActions[p.Action] {
		writeError(w, http.StatusBadRequest, "invalid_file_action", "unknown file action")
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	res, err := s.fileCall(r.Context(), site, p)
	if err != nil {
		s.writeFileError(w, r, err)
		return
	}
	if mutatingFileActions[p.Action] {
		target := p.Path
		if target == "" && len(p.Paths) > 0 {
			target = p.Paths[0]
		}
		s.audit(r, "files."+p.Action, site.Domain+":"+target, map[string]any{"site_id": site.ID, "to": p.To, "count": len(p.Paths)})
	}
	writeJSON(w, http.StatusOK, res)
}

// handleUpload receives one piece of a file as the raw request body. The browser sends a
// file in pieces of FileChunkSize; the last piece has final=1 and moves the file into place.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || offset < 0 || q.Get("path") == "" {
		writeError(w, http.StatusBadRequest, "invalid_upload", "path and offset are required")
		return
	}
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, agentproto.FileChunkSize))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "send at most 512 KB per piece")
		return
	}
	final := q.Get("final") == "1"
	res, err := s.fileCall(r.Context(), site, agentproto.FilesParams{
		Action: "write_chunk", Path: q.Get("path"), Offset: offset, Data: data, Final: final, Replace: q.Get("replace") == "1",
	})
	if err != nil {
		s.writeFileError(w, r, err)
		return
	}
	if final {
		s.audit(r, "files.upload", site.Domain+":"+q.Get("path"), map[string]any{"site_id": site.ID, "size": res.Size})
	}
	writeJSON(w, http.StatusOK, res)
}

// handleDownload streams a file to the browser, reading it from the agent in pieces.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	first, err := s.fileCall(r.Context(), site, agentproto.FilesParams{Action: "read_chunk", Path: p})
	if err != nil {
		s.writeFileError(w, r, err)
		return
	}
	name := path.Base(path.Clean("/" + p))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Length", strconv.FormatInt(first.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(first.Data); err != nil {
		return
	}
	offset := int64(len(first.Data))
	for !first.EOF && offset < first.Size {
		chunk, err := s.fileCall(r.Context(), site, agentproto.FilesParams{Action: "read_chunk", Path: p, Offset: offset})
		if err != nil {
			s.log.Warn("download interrupted", "site", site.ID, "path", p, "err", err)
			return // headers are sent; the browser sees a short download
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		offset += int64(len(chunk.Data))
		if chunk.EOF || len(chunk.Data) == 0 {
			break
		}
	}
	if offset != first.Size {
		s.log.Warn("download size changed while reading", "site", site.ID, "path", p, "want", first.Size, "got", offset)
	}
}
