package api

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	_ "image/gif"
	_ "image/jpeg"

	"github.com/go-chi/chi/v5"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// White-label: the panel's own name, color, logo and domain, set by administrators, and a
// reseller's (when an administrator allows it), seen by the reseller and their customers.

// sourceURL is where the panel's source code is offered to everyone who uses it (AGPL).
const sourceURL = "https://github.com/ekPanel/ekpanel"

// brandView is the look of the panel for one request.
type brandView struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`    // "#rrggbb", or "" for ekPanel's colors
	LogoURL *string `json:"logo_url"` // nil: no picture (ekPanel's own logo when not custom)
	// Custom is false while the panel looks like ekPanel.
	Custom bool `json:"custom"`
	// URL is the brand's own address, such as https://panel.example.com.
	URL       *string `json:"url"`
	SourceURL string  `json:"source_url"`
}

func logoURL(id string, version int) string {
	return "/api/v1/branding/logo/" + id + "?v=" + strconv.Itoa(version)
}

func viewOf(e store.EffectiveBrand) brandView {
	v := brandView{Name: e.Name, Color: e.Color, Custom: e.Custom, SourceURL: sourceURL}
	if e.LogoID != "" {
		u := logoURL(e.LogoID, e.LogoVersion)
		v.LogoURL = &u
	}
	if e.Domain != nil {
		u := "https://" + *e.Domain
		v.URL = &u
	}
	return v
}

// requestHost is the host name the request came to, lower case and without a port.
func requestHost(r *http.Request) string {
	h := r.Host
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// brandName is the panel's name as the signed-in user sees it, for authenticator apps.
func (s *Server) brandName(r *http.Request) string {
	if u := currentUser(r); u != nil {
		if e, err := s.store.EffectiveBrand(r.Context(), &u.ID, ""); err == nil {
			return e.Name
		}
	}
	return "ekPanel"
}

// handleBranding is public: the sign-in page needs it. Signed in, the look follows the
// account; before that, the domain the panel was opened at.
func (s *Server) handleBranding(w http.ResponseWriter, r *http.Request) {
	var e store.EffectiveBrand
	var err error
	if u := currentUser(r); u != nil {
		e, err = s.store.EffectiveBrand(r.Context(), &u.ID, "")
	} else {
		e, err = s.hostBrand(r)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, viewOf(e))
}

// hostBrand is the brand of the domain the request came to, for visitors not signed in.
func (s *Server) hostBrand(r *http.Request) (store.EffectiveBrand, error) {
	host := requestHost(r)
	// A domain the server's own Caddyfile serves is the panel's, whatever a brand says.
	var st tasks.PanelDomainsStatus
	if ok, _ := s.store.GetSetting(r.Context(), tasks.PanelDomainsStatusKey, &st); ok && slices.Contains(st.Skipped, host) {
		host = ""
	}
	return s.store.EffectiveBrand(r.Context(), nil, host)
}

// handleBrandLogo serves a logo. Its address carries the version, so it can be cached.
func (s *Server) handleBrandLogo(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !isUUID(id) {
		writeError(w, http.StatusNotFound, "not_found", "no such logo")
		return
	}
	logo, version, err := s.store.BrandLogo(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "no such logo")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if r.URL.Query().Get("v") == strconv.Itoa(version) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(logo)
}

// ---- Settings ----

type brandSettings struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	Domain  *string `json:"domain"`
	LogoURL *string `json:"logo_url"`
	// Allowed is false for a reseller an administrator has not allowed white-label.
	Allowed bool `json:"allowed"`
	// DomainState: "" (no domain), "active", "pending", "failed" or "no_server" (the
	// panel's server is not in the panel, so nothing serves the domain).
	DomainState string `json:"domain_state"`
	DomainError string `json:"domain_error,omitempty"` // administrators only
	// Addresses are the panel's server's public addresses, for the domain's DNS records.
	Addresses []string `json:"addresses"`
	// Panel is the panel's brand as resellers' customers fall back to (resellers only).
	Panel *brandView `json:"panel,omitempty"`
}

type brandRequest struct {
	Name   string `json:"name"`
	Color  string `json:"color"`
	Domain string `json:"domain"`
}

// brandOwner is whose brand the caller manages: nil for the panel's (administrators), or
// the reseller's own ID.
func brandOwner(r *http.Request) *string {
	if u := currentUser(r); u.Role == "reseller" {
		return &u.ID
	}
	return nil
}

// whiteLabelAllowed writes 403 for a reseller who may not use white-label.
func (s *Server) whiteLabelAllowed(w http.ResponseWriter, r *http.Request) bool {
	owner := brandOwner(r)
	if owner == nil {
		return true
	}
	l, err := s.store.GetResellerLimits(r.Context(), *owner)
	if err != nil {
		s.internalError(w, r, err)
		return false
	}
	if !l.WhiteLabel {
		writeError(w, http.StatusForbidden, "white_label_off", "an administrator has not allowed you your own branding")
		return false
	}
	return true
}

func (s *Server) handleGetBrand(w http.ResponseWriter, r *http.Request) {
	s.writeBrandSettings(w, r)
}

func (s *Server) writeBrandSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	owner := brandOwner(r)
	out := brandSettings{Allowed: true, Addresses: []string{}}
	if owner != nil {
		l, err := s.store.GetResellerLimits(ctx, *owner)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out.Allowed = l.WhiteLabel
		e, err := s.store.EffectiveBrand(ctx, nil, "")
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		v := viewOf(e)
		out.Panel = &v
	}
	b, err := s.store.GetBrand(ctx, owner)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	out.Name, out.Color, out.Domain = b.Name, b.Color, b.Domain
	if b.HasLogo {
		u := logoURL(b.ID, b.LogoVersion)
		out.LogoURL = &u
	}
	srv, _, err := s.opts.Tasks.PanelHost(ctx)
	switch {
	case err != nil:
		s.internalError(w, r, err)
		return
	case srv.ID == "":
		if out.Domain != nil {
			out.DomainState = "no_server"
		}
	default:
		if srv.PublicIPv4 != nil {
			out.Addresses = append(out.Addresses, *srv.PublicIPv4)
		}
		if srv.PublicIPv6 != nil {
			out.Addresses = append(out.Addresses, *srv.PublicIPv6)
		}
		if len(out.Addresses) == 0 {
			out.Addresses = append(out.Addresses, srv.Addresses...)
		}
		if out.Domain != nil {
			var st tasks.PanelDomainsStatus
			ok, _ := s.store.GetSetting(ctx, tasks.PanelDomainsStatusKey, &st)
			served := slices.Contains(st.Domains, *out.Domain)
			switch {
			case !ok || !served || !out.Allowed:
				out.DomainState = "pending"
			case !st.OK:
				out.DomainState = "failed"
				if owner == nil {
					out.DomainError = st.Error
				}
			default:
				out.DomainState = "active"
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

var colorRE = regexp.MustCompile(`^#[0-9a-f]{6}$`)

func (s *Server) handleSaveBrand(w http.ResponseWriter, r *http.Request) {
	var req brandRequest
	if !readJSON(w, r, &req) || !s.whiteLabelAllowed(w, r) {
		return
	}
	owner := brandOwner(r)
	req.Name = strings.TrimSpace(req.Name)
	req.Color = strings.ToLower(strings.TrimSpace(req.Color))
	switch {
	case utf8.RuneCountInString(req.Name) > 60 || strings.ContainsAny(req.Name, "<>\r\n"):
		writeError(w, http.StatusBadRequest, "invalid_brand_name", "the name can be up to 60 characters")
		return
	case owner != nil && req.Name == "":
		writeError(w, http.StatusBadRequest, "invalid_brand_name", "give your panel a name")
		return
	case req.Color != "" && !colorRE.MatchString(req.Color):
		writeError(w, http.StatusBadRequest, "invalid_color", "the color must look like #1a7f4b")
		return
	}
	var domain *string
	if d := strings.TrimSpace(req.Domain); d != "" {
		ascii := normalizeHostname(strings.ToLower(d))
		if ascii == "" {
			writeError(w, http.StatusBadRequest, "invalid_domain", "enter a domain such as panel.example.com")
			return
		}
		if owner != nil && s.panelsOwnHost(r, ascii) {
			writeError(w, http.StatusConflict, "domain_taken", "this is the panel's own domain")
			return
		}
		domain = &ascii
	}
	before, _ := s.store.GetBrand(r.Context(), owner)
	b, err := s.store.SaveBrand(r.Context(), owner, req.Name, req.Color, domain)
	var taken store.ErrHostnameTaken
	switch {
	case errors.As(err, &taken):
		writeError(w, http.StatusConflict, "domain_taken", fmt.Sprintf("%s is already used by a website, an app or another panel", taken.Hostname))
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "brand.updated", b.Name, map[string]any{"domain": deref(b.Domain), "reseller_id": deref(owner)})
	if deref(before.Domain) != deref(b.Domain) {
		s.reapplyPanelServer(r)
	}
	s.writeBrandSettings(w, r)
}

// panelsOwnHost reports whether host is an address the panel already answers at for
// everyone: its notification link address, or the address this request came to.
func (s *Server) panelsOwnHost(r *http.Request, host string) bool {
	if requestHost(r) == host {
		return true
	}
	if s.opts.Notify == nil {
		return false
	}
	if set, err := s.opts.Notify.Settings(r.Context()); err == nil && set.PanelURL != "" {
		if u, err := url.Parse(set.PanelURL); err == nil && strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

// reapplyPanelServer has the panel's server pick up a changed list of the panel's domains.
func (s *Server) reapplyPanelServer(r *http.Request) {
	if srv, _, err := s.opts.Tasks.PanelHost(r.Context()); err == nil && srv.ID != "" {
		s.queueReconcile(r, srv.ID)
	}
}

const (
	maxLogoBytes  = 2 << 20
	maxLogoPixels = 40_000_000
	maxLogoWidth  = 1024
	maxLogoHeight = 512
)

// handleSetBrandLogo takes a PNG, JPEG, GIF or WebP picture. It is decoded and written
// out again as a PNG of at most 1024×512 pixels, so only plain pixels are ever served.
func (s *Server) handleSetBrandLogo(w http.ResponseWriter, r *http.Request) {
	if !s.whiteLabelAllowed(w, r) {
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLogoBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "logo_too_large", "the logo can be up to 2 MB")
		return
	}
	out, err := logoPNG(data)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_logo", err.Error())
		return
	}
	owner := brandOwner(r)
	b, err := s.store.GetBrand(r.Context(), owner)
	if errors.Is(err, store.ErrNotFound) {
		if owner != nil {
			writeError(w, http.StatusConflict, "brand_name_first", "save your panel's name first")
			return
		}
		b, err = s.store.SaveBrand(r.Context(), nil, "", "", nil)
	}
	if err == nil {
		b, err = s.store.SetBrandLogo(r.Context(), b.ID, out)
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "brand.logo_changed", b.Name, map[string]any{"reseller_id": deref(owner)})
	s.writeBrandSettings(w, r)
}

func (s *Server) handleDeleteBrandLogo(w http.ResponseWriter, r *http.Request) {
	owner := brandOwner(r)
	b, err := s.store.GetBrand(r.Context(), owner)
	if err == nil && b.HasLogo {
		_, err = s.store.SetBrandLogo(r.Context(), b.ID, nil)
		if err == nil {
			s.audit(r, "brand.logo_removed", b.Name, map[string]any{"reseller_id": deref(owner)})
		}
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	s.writeBrandSettings(w, r)
}

// logoPNG checks a picture and writes it out as a PNG, scaled down to fit 1024×512.
func logoPNG(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("upload a PNG, JPEG, GIF or WebP picture")
	}
	if cfg.Width < 16 || cfg.Height < 16 || cfg.Width*cfg.Height > maxLogoPixels {
		return nil, errors.New("the picture must be at least 16×16 pixels and not huge")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("the picture could not be read")
	}
	b := img.Bounds()
	if scale := min(float64(maxLogoWidth)/float64(b.Dx()), float64(maxLogoHeight)/float64(b.Dy())); scale < 1 {
		dst := image.NewNRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
