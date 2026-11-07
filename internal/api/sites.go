package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"golang.org/x/net/idna"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/store"
)

const maxAliases = 20

var hostnameRE = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+([a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)

// idnaProfile converts names typed in any script (例子.测试, উদাহরণ.বাংলা) to the ASCII
// form DNS and certificates use, applying the same checks browsers do.
var idnaProfile = idna.New(idna.MapForLookup(), idna.BidiRule(), idna.Transitional(false))

var displayProfile = idna.New(idna.MapForLookup(), idna.Transitional(false))

// withDisplay adds the Unicode form of each host name, so 'xn--…' names show in the script
// their owners typed.
func withDisplay(sites ...*store.Site) {
	for _, st := range sites {
		st.DomainDisplay = toDisplay(st.Domain)
		st.AliasesDisplay = make([]string, len(st.Aliases))
		for i, a := range st.Aliases {
			st.AliasesDisplay[i] = toDisplay(a)
		}
	}
}

func toDisplay(h string) string {
	if u, err := displayProfile.ToUnicode(h); err == nil {
		return u
	}
	return h
}

// normalizeHostname returns the ASCII form of a host name, or "" if it is not valid for a site.
func normalizeHostname(h string) string {
	h = strings.TrimSuffix(strings.TrimSpace(h), ".")
	if h == "" || strings.ContainsAny(h, "*/:@ ") || net.ParseIP(h) != nil {
		return ""
	}
	ascii, err := idnaProfile.ToASCII(h)
	if err != nil || len(ascii) > 253 || !hostnameRE.MatchString(ascii) {
		return ""
	}
	return ascii
}

type siteRequest struct {
	ServerID   string   `json:"server_id"`
	Domain     string   `json:"domain"`
	Aliases    []string `json:"aliases"`
	Kind       string   `json:"kind"`
	PHPVersion *string  `json:"php_version"`
	TLS        string   `json:"tls"`
}

// validateSiteSettings checks everything except the domain and server, which only
// matter when a site is created. It writes the error response and returns false on failure.
func validateSiteSettings(w http.ResponseWriter, domain string, req *siteRequest) bool {
	aliases := make([]string, 0, len(req.Aliases))
	for _, a := range req.Aliases {
		if strings.TrimSpace(a) == "" {
			continue
		}
		n := normalizeHostname(a)
		if n == "" {
			writeError(w, http.StatusBadRequest, "invalid_alias", "one of the extra domains is not a valid domain name: "+a)
			return false
		}
		if n != domain && !slices.Contains(aliases, n) {
			aliases = append(aliases, n)
		}
	}
	if len(aliases) > maxAliases {
		writeError(w, http.StatusBadRequest, "too_many_aliases", "a site can have at most 20 extra domains")
		return false
	}
	req.Aliases = aliases

	switch req.Kind {
	case "static":
		req.PHPVersion = nil
	case "php":
		if req.PHPVersion == nil || !slices.Contains(agentproto.PHPVersions, *req.PHPVersion) {
			writeError(w, http.StatusBadRequest, "invalid_php_version", "choose one of the supported PHP versions")
			return false
		}
	default:
		writeError(w, http.StatusBadRequest, "invalid_site_kind", "a site is either static or PHP")
		return false
	}
	if req.TLS == "" {
		req.TLS = "auto"
	}
	if req.TLS != "auto" && req.TLS != "internal" {
		writeError(w, http.StatusBadRequest, "invalid_tls", "SSL must be automatic or self-signed")
		return false
	}
	return true
}

// linuxUserFor derives a Linux account name from a domain: up to 12 letters and digits
// of its first label, then a random suffix. "my-shop.example.com" becomes "myshop_3fa9".
func linuxUserFor(domain string) string {
	label, _, _ := strings.Cut(domain, ".")
	label = strings.TrimPrefix(label, "xn--")
	var b strings.Builder
	for _, r := range label {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	base := b.String()
	if base == "" || base[0] < 'a' {
		base = "site" + base
	}
	if len(base) > 12 {
		base = base[:12]
	}
	suffix := make([]byte, 2)
	rand.Read(suffix)
	return base + "_" + hex.EncodeToString(suffix)
}

func (s *Server) writeSiteStoreError(w http.ResponseWriter, r *http.Request, err error) {
	var taken store.ErrHostnameTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, "hostname_taken", taken.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "site_not_found", "no site with that ID")
	default:
		s.internalError(w, r, err)
	}
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	serverFilter := r.URL.Query().Get("server_id")
	if serverFilter != "" && !isUUID(serverFilter) {
		writeJSON(w, http.StatusOK, []store.Site{})
		return
	}
	list, err := s.store.ListSites(r.Context(), serverFilter)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for i := range list {
		withDisplay(&list[i])
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateSite(w http.ResponseWriter, r *http.Request) {
	var req siteRequest
	if !readJSON(w, r, &req) {
		return
	}
	domain := normalizeHostname(req.Domain)
	if domain == "" {
		writeError(w, http.StatusBadRequest, "invalid_domain", "enter a domain name such as example.com")
		return
	}
	if !validateSiteSettings(w, domain, &req) {
		return
	}
	if !isUUID(req.ServerID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	srv, err := s.store.GetServer(r.Context(), req.ServerID)
	if s.serverLookupFailed(w, r, err) {
		return
	}

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
	site, err := s.store.CreateSite(r.Context(), store.NewSite{
		ServerID: srv.ID, Domain: domain, Aliases: req.Aliases, Kind: req.Kind, PHPVersion: req.PHPVersion,
		TLS: req.TLS, LinuxUser: user, CreatedBy: &u.ID,
	})
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.created", site.Domain, map[string]any{"site_id": site.ID, "server_id": srv.ID})
	s.queueReconcile(r, srv.ID)
	withDisplay(&site)
	writeJSON(w, http.StatusCreated, site)
}

func (s *Server) queueReconcile(r *http.Request, serverID string) {
	u := currentUser(r)
	if _, err := s.opts.Tasks.QueueReconcile(r.Context(), serverID, &u.ID); err != nil {
		s.log.Error("queue reconcile", "server", serverID, "err", err)
	}
}

func siteID(r *http.Request) string {
	if id := chi.URLParam(r, "id"); isUUID(id) {
		return id
	}
	return nilUUID
}

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	site, err := s.store.GetSite(r.Context(), siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) handleUpdateSite(w http.ResponseWriter, r *http.Request) {
	var req siteRequest
	if !readJSON(w, r, &req) {
		return
	}
	current, err := s.store.GetSite(r.Context(), siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	if current.State == "deleting" {
		writeError(w, http.StatusConflict, "site_deleting", "this site is being deleted")
		return
	}
	if !validateSiteSettings(w, current.Domain, &req) {
		return
	}
	site, err := s.store.UpdateSite(r.Context(), current.ID, store.SiteChange{
		Aliases: req.Aliases, Kind: req.Kind, PHPVersion: req.PHPVersion, TLS: req.TLS,
	})
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.updated", site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusOK, site)
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	site, err := s.store.GetSite(r.Context(), siteID(r))
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	site, err = s.store.MarkSiteDeleting(r.Context(), site.ID)
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	s.audit(r, "site.deleted", site.Domain, map[string]any{"site_id": site.ID})
	s.queueReconcile(r, site.ServerID)
	withDisplay(&site)
	writeJSON(w, http.StatusAccepted, site)
}

func (s *Server) handlePHPVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"versions": agentproto.PHPVersions, "default": agentproto.DefaultPHPVersion})
}
