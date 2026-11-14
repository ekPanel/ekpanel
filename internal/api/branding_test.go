package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/api"
	"ekpanel/internal/notify"
	"ekpanel/internal/store"
)

type brandBody struct {
	Name    string  `json:"name"`
	Color   string  `json:"color"`
	LogoURL *string `json:"logo_url"`
	Custom  bool    `json:"custom"`
	URL     *string `json:"url"`
}

type brandSettingsBody struct {
	Name        string   `json:"name"`
	Color       string   `json:"color"`
	Domain      *string  `json:"domain"`
	LogoURL     *string  `json:"logo_url"`
	Allowed     bool     `json:"allowed"`
	DomainState string   `json:"domain_state"`
	Addresses   []string `json:"addresses"`
}

// brandingAt asks for the look of the panel opened at host, signed out.
func brandingAt(t *testing.T, base, host string) brandBody {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/api/v1/branding", nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b brandBody
	json.NewDecoder(resp.Body).Decode(&b)
	return b
}

func pngOf(w, h int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, h/2, color.NRGBA{R: 200, A: 255})
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func TestBranding(t *testing.T) {
	p := newTestPanel(t)
	if b := brandingAt(t, p.web.URL, ""); b.Name != "ekPanel" || b.Custom || b.LogoURL != nil {
		t.Fatalf("default brand: %+v", b)
	}
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	var noServer brandSettingsBody
	if admin.do("PUT", "/api/v1/brand", map[string]string{"name": "Host BD", "domain": "panel.hostbd.test"}, &noServer); noServer.DomainState != "no_server" {
		t.Errorf("domain without the panel's server: %+v", noServer)
	}
	serverID, cfg, _, _ := enrolled(t, p, admin, "panel-server")
	var mu sync.Mutex
	var panel *agentproto.PanelSpec
	conn := connectAgent(t, p, cfg, "dev", true, func(op string, raw json.RawMessage) (any, *agentproto.OpError) {
		var in agentproto.StateApplyParams
		json.Unmarshal(raw, &in)
		mu.Lock()
		panel = in.Panel
		mu.Unlock()
		res := agentproto.StateApplyResult{}
		for _, s := range in.Sites {
			res.Sites = append(res.Sites, agentproto.SiteResult{ID: s.ID, Generation: s.Generation, OK: true})
		}
		if in.Panel != nil {
			res.Panel = &agentproto.PanelResult{OK: true}
		}
		return res, nil
	})
	defer conn.CloseNow()
	servedDomains := func(what string, want ...string) {
		t.Helper()
		eventually(t, what, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return panel != nil && panel.Upstream == "127.0.0.1:8080" && slices.Equal(panel.Domains, want)
		})
	}
	var e errBody

	// ---- The panel's own brand ----
	var set brandSettingsBody
	if st := admin.do("PUT", "/api/v1/brand", map[string]string{"name": "Host BD", "color": "#12AB34", "domain": "panel.hostbd.test"}, &set); st != http.StatusOK || set.Name != "Host BD" || set.Color != "#12ab34" {
		t.Fatalf("save brand: %d %+v", st, set)
	}
	servedDomains("panel domain sent to the panel's server", "panel.hostbd.test")
	eventually(t, "domain active", func() bool {
		admin.do("GET", "/api/v1/brand", nil, &set)
		return set.DomainState == "active"
	})
	for _, bad := range []map[string]string{{"name": "x", "color": "green"}, {"name": "<b>", "color": ""}, {"name": "x", "domain": "not a domain"}} {
		if st := admin.do("PUT", "/api/v1/brand", bad, &e); st != http.StatusBadRequest {
			t.Errorf("accepted %v: %d", bad, st)
		}
	}

	// Logo: decoded and written out again as a PNG of at most 1024×512.
	if st := admin.do("PUT", "/api/v1/brand/logo", nil, &e); st != http.StatusBadRequest {
		t.Errorf("empty logo: %d", st)
	}
	upload := func(c *client, data []byte) (int, brandSettingsBody) {
		req, _ := http.NewRequest("PUT", p.web.URL+"/api/v1/brand/logo", bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := c.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out brandSettingsBody
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if st, _ := upload(admin, []byte("<svg onload=alert(1)>")); st != http.StatusBadRequest {
		t.Errorf("SVG accepted: %d", st)
	}
	st, withLogo := upload(admin, pngOf(2048, 128))
	if st != http.StatusOK || withLogo.LogoURL == nil {
		t.Fatalf("logo: %d %+v", st, withLogo)
	}
	resp, err := http.Get(p.web.URL + *withLogo.LogoURL)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(resp.Body)
	resp.Body.Close()
	if err != nil || img.Bounds().Dx() != 1024 || img.Bounds().Dy() != 64 || resp.Header.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("served logo: %v %v %q", err, img.Bounds(), resp.Header.Get("Cache-Control"))
	}
	b := brandingAt(t, p.web.URL, "")
	if b.Name != "Host BD" || b.Color != "#12ab34" || !b.Custom || b.LogoURL == nil || b.URL == nil || *b.URL != "https://panel.hostbd.test" {
		t.Fatalf("panel brand: %+v", b)
	}

	// ---- A reseller ----
	limits := map[string]any{"max_customers": 2, "max_sites": 2, "max_databases": 1, "server_ids": []string{serverID}, "sftp": true}
	var made struct {
		Reseller struct {
			ID string `json:"id"`
		} `json:"reseller"`
		Password string `json:"password"`
	}
	admin.do("POST", "/api/v1/resellers", map[string]any{"email": "r@example.com", "name": "R", "limits": limits}, &made)
	rid := made.Reseller.ID
	r := login(t, p.web.URL, "r@example.com", made.Password)
	if r.do("GET", "/api/v1/brand", nil, &set); set.Allowed {
		t.Error("white-label allowed by default")
	}
	if st := r.do("PUT", "/api/v1/brand", map[string]string{"name": "Reseller One"}, &e); st != http.StatusForbidden || e.Code != "white_label_off" {
		t.Errorf("brand without white-label: %d %s", st, e.Code)
	}
	limits["white_label"] = true
	admin.do("PUT", "/api/v1/resellers/"+rid, map[string]any{"email": "r@example.com", "name": "R", "limits": limits}, nil)
	if st := r.do("PUT", "/api/v1/brand", map[string]string{"name": "Reseller One", "domain": "panel.hostbd.test"}, &e); st != http.StatusConflict || e.Code != "domain_taken" {
		t.Errorf("another brand's domain: %d %s", st, e.Code)
	}
	if st := r.do("PUT", "/api/v1/brand", map[string]string{"name": "", "domain": "cp.reseller.test"}, &e); st != http.StatusBadRequest {
		t.Errorf("reseller brand without a name: %d", st)
	}
	if st := r.do("PUT", "/api/v1/brand", map[string]string{"name": "Reseller One", "domain": "cp.reseller.test"}, &set); st != http.StatusOK {
		t.Fatalf("reseller brand: %d", st)
	}
	servedDomains("reseller domain sent", "cp.reseller.test", "panel.hostbd.test")

	// A website cannot take a panel's domain.
	if st := admin.do("POST", "/api/v1/sites", map[string]any{"server_id": serverID, "domain": "cp.reseller.test", "kind": "static"}, &e); st != http.StatusConflict {
		t.Errorf("website on a panel domain: %d %s", st, e.Code)
	}

	// Opened at the reseller's domain, and signed in as their customer: the reseller's
	// name, no logo (the panel's is not mixed in), the panel's color.
	b = brandingAt(t, p.web.URL, "cp.reseller.test")
	if b.Name != "Reseller One" || b.LogoURL != nil || b.Color != "#12ab34" || b.URL == nil || *b.URL != "https://cp.reseller.test" {
		t.Fatalf("at the reseller's domain: %+v", b)
	}
	var pkg, cust struct {
		ID       string `json:"id"`
		Customer struct {
			ID string `json:"id"`
		} `json:"customer"`
		Password string `json:"password"`
	}
	r.do("POST", "/api/v1/packages", map[string]any{"name": "Basic", "max_sites": 1, "max_databases": 1, "server_ids": []string{serverID}}, &pkg)
	if st := r.do("POST", "/api/v1/customers", map[string]string{"email": "c@example.com", "name": "C", "package_id": pkg.ID}, &cust); st != http.StatusCreated {
		t.Fatalf("customer: %d", st)
	}
	c := login(t, p.web.URL, "c@example.com", cust.Password)
	if c.do("GET", "/api/v1/branding", nil, &b); b.Name != "Reseller One" {
		t.Errorf("customer sees %+v", b)
	}
	if st := c.do("GET", "/api/v1/brand", nil, nil); st != http.StatusForbidden {
		t.Errorf("customer reads brand settings: %d", st)
	}
	if admin.do("GET", "/api/v1/branding", nil, &b); b.Name != "Host BD" {
		t.Errorf("administrator sees %+v", b)
	}

	// Notifications carry the reseller's name, and links on their domain.
	n := notify.New(p.store, nil, slog.New(slog.DiscardHandler))
	n.Event(context.Background(), []string{cust.Customer.ID}, "login_new_ip", "warning", map[string]string{"ip": "192.0.2.7"}, "/settings/security")
	var inbox struct {
		Notifications []struct {
			Body string `json:"body"`
			Link string `json:"link"`
		} `json:"notifications"`
	}
	c.do("GET", "/api/v1/notifications", nil, &inbox)
	if len(inbox.Notifications) != 1 || !strings.Contains(inbox.Notifications[0].Body, "Reseller One") ||
		inbox.Notifications[0].Link != "https://cp.reseller.test/settings/security" {
		t.Errorf("customer's notification: %+v", inbox)
	}

	// Taking white-label away: the domain stops being served, the customer sees the panel's brand.
	limits["white_label"] = false
	admin.do("PUT", "/api/v1/resellers/"+rid, map[string]any{"email": "r@example.com", "name": "R", "limits": limits}, nil)
	servedDomains("reseller domain withdrawn", "panel.hostbd.test")
	if c.do("GET", "/api/v1/branding", nil, &b); b.Name != "Host BD" {
		t.Errorf("customer after white-label off: %+v", b)
	}
	if b := brandingAt(t, p.web.URL, "cp.reseller.test"); b.Name != "Host BD" {
		t.Errorf("reseller's domain after white-label off: %+v", b)
	}

	// Removing the logo and the domain.
	if admin.do("DELETE", "/api/v1/brand/logo", nil, &set); set.LogoURL != nil {
		t.Error("logo not removed")
	}
	admin.do("PUT", "/api/v1/brand", map[string]string{"name": "Host BD"}, &set)
	servedDomains("panel domain removed")
}

func TestBrandedIndex(t *testing.T) {
	page := `<!doctype html><html><head><link rel="icon" type="image/svg+xml" href="/favicon.svg" />
<title>ekPanel</title><script src="/theme-init.js"></script></head><body></body></html>`
	p := newTestPanel(t, func(o *api.Options, _ *store.Store) {
		o.UI = fstest.MapFS{"index.html": {Data: []byte(page)}}
	})
	get := func() string {
		resp, err := http.Get(p.web.URL + "/sites")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(); got != page {
		t.Fatalf("unbranded page changed:\n%s", got)
	}
	admin := newClient(t, p.web.URL)
	setUp(t, admin)
	admin.do("PUT", "/api/v1/brand", map[string]string{"name": `Host "BD" & Co`, "color": "#1d4ed8"}, nil)
	got := get()
	for _, want := range []string{`<title>Host &#34;BD&#34; &amp; Co</title>`, `<meta name="ekpanel-brand-color" content="#1d4ed8" />`, `href="/favicon.svg"`} {
		if !strings.Contains(got, want) {
			t.Errorf("page lacks %q:\n%s", want, got)
		}
	}
}
