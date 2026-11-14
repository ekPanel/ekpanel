package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"ekpanel/internal/agentproto"
)

// The panel's own extra domains (white-label): one Caddy file proxying them to the panel.

const (
	panelCaddy   = caddySitesDir + "/panel-domains.caddy"
	panelID      = "panel-domains" // its result among the sites' while applying
	maxPanelDoms = 1000
)

var panelCaddyTemplate = template.Must(template.New("panel").Parse(`# Managed by ekPanel: the panel's own domains (white-label).
{{range .Domains}}
{{.Name}} {
{{- if .Internal}}
	tls internal
{{- end}}
	encode zstd gzip
	reverse_proxy {{$.Upstream}}
}
{{end}}`))

// internalTLS is for names no public certificate authority signs (tests and labs).
func internalTLS(domain string) bool {
	for _, tld := range []string{".test", ".localhost", ".internal", ".example", ".invalid"} {
		if strings.HasSuffix(domain, tld) {
			return true
		}
	}
	return false
}

func validatePanel(p agentproto.PanelSpec) error {
	host, port, err := net.SplitHostPort(p.Upstream)
	if err != nil || port == "" {
		return errors.New("invalid panel address")
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("the panel address must be on this server (127.0.0.1)")
	}
	if len(p.Domains) > maxPanelDoms {
		return errors.New("too many panel domains")
	}
	for _, d := range p.Domains {
		if len(d) > 253 || !hostnameRE.MatchString(d) {
			return fmt.Errorf("invalid panel domain %q", d)
		}
	}
	return nil
}

// caddyfileSiteRE finds a site block's addresses: a line at the top level ending in "{".
var caddyfileSiteRE = regexp.MustCompile(`(?m)^([^\s#{][^{\n]*)\{\s*$`)

// mainCaddyfileHosts lists the host names the server's own Caddyfile has site blocks for,
// such as the domain the installer set up for the panel.
func mainCaddyfileHosts(sys *system) map[string]bool {
	b, err := os.ReadFile(sys.path(caddyfile))
	if err != nil {
		return nil
	}
	hosts := map[string]bool{}
	for _, m := range caddyfileSiteRE.FindAllSubmatch(b, -1) {
		for _, addr := range strings.FieldsFunc(string(m[1]), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			addr = strings.TrimPrefix(strings.TrimPrefix(addr, "https://"), "http://")
			if h, _, err := net.SplitHostPort(addr); err == nil {
				addr = h
			}
			hosts[strings.ToLower(addr)] = true
		}
	}
	return hosts
}

// applyPanelDomains writes the panel's domains; it returns the ones skipped because the
// main Caddyfile serves them already (two site blocks for one name stop Caddy loading).
func (a *applier) applyPanelDomains(p agentproto.PanelSpec) ([]string, error) {
	if err := validatePanel(p); err != nil {
		return nil, err
	}
	main := mainCaddyfileHosts(a.sys)
	type dom struct {
		Name     string
		Internal bool
	}
	var doms []dom
	var skipped []string
	for _, d := range p.Domains {
		switch {
		case main[d]:
			skipped = append(skipped, d)
		case !slices.ContainsFunc(doms, func(x dom) bool { return x.Name == d }):
			doms = append(doms, dom{Name: d, Internal: internalTLS(d)})
		}
	}
	if len(doms) == 0 {
		a.removeCaddyFile(panelCaddy, panelID)
		return skipped, nil
	}
	var buf bytes.Buffer
	if err := panelCaddyTemplate.Execute(&buf, map[string]any{"Domains": doms, "Upstream": p.Upstream}); err != nil {
		return nil, err
	}
	changed, prev, err := a.writeIfChanged(panelCaddy, buf.Bytes())
	if err != nil {
		return nil, err
	}
	if changed {
		a.caddyChanged[panelCaddy] = prev
		a.caddySites[panelCaddy] = panelID
	}
	return skipped, nil
}
