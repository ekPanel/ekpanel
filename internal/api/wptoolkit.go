package api

import (
	"errors"
	"net/http"
	"slices"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// loadWordPress finds a site that has WordPress, as the last scan saw it.
func (s *Server) loadWordPress(w http.ResponseWriter, r *http.Request) (store.Site, bool) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return site, false
	}
	if site.App == nil || *site.App != "wordpress" {
		writeError(w, http.StatusConflict, "not_wordpress", "this site has no WordPress")
		return site, false
	}
	return site, true
}

// handleListWordPress lists the user's WordPress sites with what their last scan found.
func (s *Server) handleListWordPress(w http.ResponseWriter, r *http.Request) {
	sites, err := s.store.ListSitesOf(r.Context(), "", ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := []store.Site{}
	for _, site := range sites {
		if site.App != nil && *site.App == "wordpress" && site.State != "deleting" {
			withDisplay(&site)
			out = append(out, site)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleScanWordPress looks for WordPress on every PHP site of the user, also WordPress
// installed without ekPanel.
func (s *Server) handleScanWordPress(w http.ResponseWriter, r *http.Request) {
	sites, err := s.store.ListSitesOf(r.Context(), "", ownerScope(r))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	ids := []string{}
	for _, site := range sites {
		if site.Kind == "php" && site.State != "deleting" {
			ids = append(ids, site.ID)
		}
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueWordPressScan(r.Context(), ids, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, t)
}

// handleRefreshWordPress reads one site's WordPress again, right away.
func (s *Server) handleRefreshWordPress(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if site.Kind != "php" {
		writeError(w, http.StatusConflict, "wordpress_needs_php", "WordPress needs a PHP site")
		return
	}
	if _, err := s.opts.Tasks.ScanWordPress(r.Context(), site); err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	s.writeSite(w, r, site.ID)
}

func (s *Server) writeSite(w http.ResponseWriter, r *http.Request, id string) {
	site, err := s.store.GetSite(r.Context(), id)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

type wpUpdateRequest struct {
	Core    string   `json:"core"`
	Plugins []string `json:"plugins"`
	Themes  []string `json:"themes"`
}

func (s *Server) handleUpdateWordPress(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadWordPress(w, r)
	if !ok {
		return
	}
	var req wpUpdateRequest
	if !readJSON(w, r, &req) {
		return
	}
	valid := slices.Contains([]string{"", "minor", "latest"}, req.Core) && (req.Core != "" || len(req.Plugins) > 0 || len(req.Themes) > 0)
	for _, n := range append(slices.Clone(req.Plugins), req.Themes...) {
		valid = valid && (n == "*" || agentproto.ValidWordPressSlug(n))
	}
	if !valid {
		writeError(w, http.StatusBadRequest, "invalid_wp_update", "choose what to update")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueWordPressUpdate(r.Context(), site, tasks.WordPressUpdate{Core: req.Core, Plugins: req.Plugins, Themes: req.Themes}, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_update", site.Domain, map[string]any{"site_id": site.ID, "task_id": t.ID, "core": req.Core,
		"plugins": req.Plugins, "themes": req.Themes})
	writeJSON(w, http.StatusAccepted, t)
}

type wpManageRequest struct {
	Action string `json:"action"`
	Name   string `json:"name"`
}

func (s *Server) handleManageWordPress(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadWordPress(w, r)
	if !ok {
		return
	}
	var req wpManageRequest
	if !readJSON(w, r, &req) {
		return
	}
	if !slices.Contains(agentproto.WordPressActions, req.Action) || (req.Name != "" && !agentproto.ValidWordPressSlug(req.Name)) {
		writeError(w, http.StatusBadRequest, "invalid_wp_action", "unknown change")
		return
	}
	if _, err := s.opts.Tasks.ManageWordPress(r.Context(), site, req.Action, req.Name); err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_"+req.Action, site.Domain, map[string]any{"site_id": site.ID, "name": req.Name})
	s.writeSite(w, r, site.ID)
}

func (s *Server) handleSetWordPressAuto(w http.ResponseWriter, r *http.Request) {
	site, ok := s.loadWordPress(w, r)
	if !ok {
		return
	}
	var req store.WordPressAuto
	if !readJSON(w, r, &req) {
		return
	}
	if !slices.Contains([]string{"off", "minor", "all"}, req.Core) {
		writeError(w, http.StatusBadRequest, "invalid_wp_auto", "choose which core updates to make")
		return
	}
	if err := s.store.SetWordPressAuto(r.Context(), site.ID, req); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_auto_updates", site.Domain, map[string]any{"site_id": site.ID, "core": req.Core, "plugins": req.Plugins, "themes": req.Themes})
	s.writeSite(w, r, site.ID)
}

// handleCreateStaging makes a staging copy at staging.<domain>: a website and database of
// its own (counted in the owner's package), with the live site's files and content.
func (s *Server) handleCreateStaging(w http.ResponseWriter, r *http.Request) {
	live, ok := s.loadWordPress(w, r)
	if !ok {
		return
	}
	if live.StagingOf != nil {
		writeError(w, http.StatusConflict, "is_staging", "this site is itself a staging copy")
		return
	}
	if _, err := s.store.StagingOf(r.Context(), live.ID); err == nil {
		writeError(w, http.StatusConflict, "staging_exists", "this site already has a staging copy")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	pkg, err := s.packageFor(r, live.OwnerID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	place := placement{OwnerID: live.OwnerID, Package: pkg}
	if !s.allowNew(w, r, place, "site", live.ServerID) || !s.allowNew(w, r, place, "database", live.ServerID) {
		return
	}
	domain := "staging." + live.Domain
	user := ""
	for range 10 {
		candidate := linuxUserFor(domain)
		taken, err := s.store.LinuxUserTaken(r.Context(), candidate)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if !taken {
			user = candidate
			break
		}
	}
	if user == "" {
		s.internalError(w, r, errors.New("could not find a free Linux account name"))
		return
	}
	u := currentUser(r)
	staging, err := s.store.CreateSite(r.Context(), store.NewSite{ServerID: live.ServerID, Domain: domain, Aliases: []string{}, Kind: "php",
		PHPVersion: live.PHPVersion, TLS: stagingTLS(live.TLS), LinuxUser: user, OwnerID: live.OwnerID, CreatedBy: &u.ID})
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if err := s.store.SetStagingOf(r.Context(), staging.ID, live.ID); err != nil {
		s.internalError(w, r, err)
		return
	}
	t, err := s.opts.Tasks.QueueWordPressStaging(r.Context(), live, staging, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_staging", live.Domain, map[string]any{"site_id": live.ID, "staging_id": staging.ID, "task_id": t.ID})
	if has, err := s.store.HasZones(r.Context()); err == nil && has {
		s.queueDNSSync(r, "")
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"task": t, "staging_id": staging.ID, "domain": domain})
}

// stagingTLS is the copy's certificate: a site's own certificate does not cover the
// staging name, so that one gets a free certificate instead.
func stagingTLS(live string) string {
	if live == "custom" {
		return "auto"
	}
	return live
}

type wpCopyRequest struct {
	Direction string `json:"direction"` // "push" (staging over live) or "pull" (live over staging)
	Files     bool   `json:"files"`
	Database  bool   `json:"database"`
}

func (s *Server) handleCopyStaging(w http.ResponseWriter, r *http.Request) {
	staging, ok := s.loadSite(w, r)
	if !ok {
		return
	}
	if staging.StagingOf == nil {
		writeError(w, http.StatusConflict, "not_staging", "this site is not a staging copy")
		return
	}
	var req wpCopyRequest
	if !readJSON(w, r, &req) {
		return
	}
	if (req.Direction != "push" && req.Direction != "pull") || (!req.Files && !req.Database) {
		writeError(w, http.StatusBadRequest, "invalid_wp_copy", "choose a direction and what to copy")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueueWordPressCopy(r.Context(), staging, tasks.WordPressCopy{Direction: req.Direction, Files: req.Files, Database: req.Database}, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "site.wordpress_"+req.Direction, staging.Domain, map[string]any{"site_id": staging.ID, "live_id": *staging.StagingOf,
		"files": req.Files, "database": req.Database, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
