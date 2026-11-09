package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"slices"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

// handleSetSiteWeb replaces a site's redirects, hotlink protection and PHP settings.
func (s *Server) handleSetSiteWeb(w http.ResponseWriter, r *http.Request) {
	var web agentproto.SiteWeb
	if !readJSON(w, r, &web) {
		return
	}
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if h := web.Hotlink; h != nil {
		for i := range h.Extensions {
			h.Extensions[i] = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(h.Extensions[i])), ".")
		}
		for i := range h.Allowed {
			h.Allowed[i] = strings.ToLower(strings.TrimSpace(h.Allowed[i]))
		}
	}
	for k, v := range web.PHP {
		if v = strings.TrimSpace(v); v == "" || (agentproto.PHPSettings[k].Default == v) {
			delete(web.PHP, k) // the default needs no setting of its own
		} else {
			web.PHP[k] = v
		}
	}
	if err := web.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_site_web", err.Error())
		return
	}
	if site.Kind != "php" && len(web.PHP) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_site_web", "only PHP sites have PHP settings")
		return
	}
	site, err = s.store.SetSiteWeb(r.Context(), site.ID, web)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.web_updated", site.Domain, map[string]any{"site_id": site.ID, "redirects": len(web.Redirects),
		"hotlink": web.Hotlink != nil, "php_settings": len(web.PHP)})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

// handleSetSiteCert makes a site use an uploaded certificate, after checking that it fits
// its key, is valid now and covers the site's domain.
func (s *Server) handleSetSiteCert(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cert string `json:"cert_pem"`
		Key  string `json:"key_pem"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	fail := func(msg string) { writeError(w, http.StatusBadRequest, "invalid_certificate", msg) }
	if len(req.Cert) > 64<<10 || len(req.Key) > 16<<10 {
		fail("the certificate or key is too large")
		return
	}
	pair, err := tls.X509KeyPair([]byte(req.Cert), []byte(req.Key))
	if err != nil {
		fail("the certificate and key must be PEM files that belong together: " + err.Error())
		return
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		fail(err.Error())
		return
	}
	now := time.Now()
	switch {
	case now.Before(leaf.NotBefore):
		fail("the certificate is not valid yet")
		return
	case now.After(leaf.NotAfter):
		fail("the certificate has expired")
		return
	case leaf.VerifyHostname(site.Domain) != nil:
		fail("the certificate is not for " + site.Domain)
		return
	}
	uncovered := []string{}
	for _, a := range site.Aliases {
		if leaf.VerifyHostname(a) != nil {
			uncovered = append(uncovered, a)
		}
	}
	names := slices.Clone(leaf.DNSNames)
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}
	site, err = s.store.SetSiteCert(r.Context(), site.ID, &store.SiteCert{Chain: strings.TrimSpace(req.Cert) + "\n",
		SealedKey: s.opts.Secrets.Seal([]byte(strings.TrimSpace(req.Key) + "\n")), ExpiresAt: leaf.NotAfter, Names: names})
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.certificate_uploaded", site.Domain, map[string]any{"site_id": site.ID, "expires": leaf.NotAfter, "names": names})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, map[string]any{"site": site, "uncovered": uncovered})
}

func (s *Server) handleDeleteSiteCert(w http.ResponseWriter, r *http.Request) {
	site, err := s.getSite(r, siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if site.TLS != "custom" {
		writeJSON(w, http.StatusOK, site)
		return
	}
	site, err = s.store.SetSiteCert(r.Context(), site.ID, nil)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.certificate_removed", site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

// ---- PHP extensions of a server ----

func (s *Server) handlePHPInfo(w http.ResponseWriter, r *http.Request) {
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, srv.ID, agentproto.OpPHPInfo, struct{}{})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(raw)
}

func (s *Server) handleSetPHPExtension(w http.ResponseWriter, r *http.Request) {
	var p agentproto.PHPExtensionParams
	if !readJSON(w, r, &p) {
		return
	}
	srv, err := s.store.GetServer(r.Context(), serverID(r))
	if s.serverLookupFailed(w, r, err) {
		return
	}
	if !slices.Contains(agentproto.PHPVersions, p.Version) || !slices.Contains(agentproto.OptionalPHPExtensions, p.Name) {
		writeError(w, http.StatusBadRequest, "invalid_php_extension", "choose an optional extension of a supported PHP version")
		return
	}
	u := currentUser(r)
	t, err := s.opts.Tasks.QueuePHPExtension(r.Context(), srv.ID, p, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "server.php_extension", srv.Name, map[string]any{"server_id": srv.ID, "version": p.Version, "extension": p.Name, "installed": p.Installed})
	writeJSON(w, http.StatusAccepted, t)
}
