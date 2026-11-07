package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// CrowdSec comes from its own package repository (the distributions' packages are years
// old). Its local API moves to crowdsecAPI: its default port 8080 is the panel's.

const (
	crowdsecAPI       = "127.0.0.1:18910"
	crowdsecList      = "/etc/apt/sources.list.d/ekpanel-crowdsec.list"
	crowdsecKeyring   = "/usr/share/keyrings/ekpanel-crowdsec.asc"
	crowdsecBouncer   = "ekpanel-firewall"
	crowdsecAllowlist = "ekpanel"
	bouncerLocal      = "/etc/crowdsec/bouncers/crowdsec-firewall-bouncer.yaml.local"
	crowdsecAcquis    = "/etc/crowdsec/acquis.d/ekpanel.yaml"
)

// crowdsecCollections are the detections ekPanel turns on: SSH, Linux, the web server's
// logs, common web attacks and known CVEs, and WordPress.
var crowdsecCollections = []string{"crowdsecurity/linux", "crowdsecurity/sshd", "crowdsecurity/caddy", "crowdsecurity/base-http-scenarios",
	"crowdsecurity/http-cve", "crowdsecurity/wordpress", "crowdsecurity/whitelist-good-actors"}

const crowdsecAcquisition = `# Managed by ekPanel. Changes here are overwritten.
# Every website's and app's access log (JSON, with the visitor's address behind Cloudflare).
filenames:
  - /var/log/caddy/*.log
labels:
  type: caddy
source: file
`

func crowdsecInstalled(ctx context.Context, sys *system) bool {
	out, _ := sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", "crowdsec")
	return strings.Contains(string(out), "install ok installed")
}

func unitActive(ctx context.Context, sys *system, unit string) bool {
	out, _ := sys.run(ctx, "systemctl", "is-active", unit)
	return strings.TrimSpace(string(out)) == "active"
}

func crowdsecStatus(ctx context.Context, sys *system) agentproto.CrowdSecStatus {
	st := agentproto.CrowdSecStatus{Installed: crowdsecInstalled(ctx, sys)}
	if !st.Installed {
		return st
	}
	st.Active = unitActive(ctx, sys, "crowdsec")
	st.BouncerActive = unitActive(ctx, sys, "crowdsec-firewall-bouncer")
	if out, err := sys.run(ctx, "cscli", "version"); err == nil {
		for line := range strings.Lines(string(out)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version:"); ok {
				// "v1.8.2-debian-pragmatic-amd64-3d369d64" -> "1.8.2"
				v, _, _ = strings.Cut(strings.TrimPrefix(strings.TrimSpace(v), "v"), "-")
				st.Version = v
			}
		}
	}
	if st.Active {
		if bans, err := listBans(ctx, sys); err == nil {
			st.Bans = len(bans)
		}
	}
	return st
}

// applySecurity runs CrowdSec or stops it (keeping its data), and keeps its allowlist.
func applySecurity(ctx context.Context, sys *system, spec agentproto.SecuritySpec) *agentproto.SecurityResult {
	res := &agentproto.SecurityResult{}
	err := func() error {
		if err := spec.Validate(); err != nil {
			return err
		}
		if !spec.CrowdSec {
			if crowdsecInstalled(ctx, sys) {
				sys.run(ctx, "systemctl", "disable", "--now", "crowdsec-firewall-bouncer", "crowdsec")
			}
			return nil
		}
		if err := ensureCrowdSec(ctx, sys); err != nil {
			return err
		}
		return syncAllowlist(ctx, sys, spec.Allowlist)
	}()
	if err != nil {
		res.Error = err.Error()
	}
	res.CrowdSec = crowdsecStatus(ctx, sys)
	return res
}

func ensureCrowdSecRepository(ctx context.Context, sys *system) error {
	if _, err := os.Stat(sys.path(crowdsecList)); err == nil {
		return nil
	}
	id, codename := sys.osID()
	if (id != "ubuntu" && id != "debian") || codename == "" {
		return fmt.Errorf("CrowdSec is not available for %s %s", id, codename)
	}
	key, err := sys.fetch(ctx, "https://packagecloud.io/crowdsec/crowdsec/gpgkey")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(key), "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		return errors.New("the CrowdSec repository key is not a PGP key")
	}
	if err := os.WriteFile(sys.path(crowdsecKeyring), key, 0o644); err != nil {
		return err
	}
	line := "deb [signed-by=" + crowdsecKeyring + "] https://packagecloud.io/crowdsec/crowdsec/" + id + " " + codename + " main\n"
	if err := os.WriteFile(sys.path(crowdsecList), []byte(line), 0o644); err != nil {
		return err
	}
	if _, err := sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "update"); err != nil {
		os.Remove(sys.path(crowdsecList))
		return err
	}
	return nil
}

// ensureCrowdSec installs CrowdSec and the firewall bouncer, configures and starts them.
func ensureCrowdSec(ctx context.Context, sys *system) error {
	a := &applier{sys: sys}
	fresh := !crowdsecInstalled(ctx, sys)
	if fresh {
		if err := ensureCrowdSecRepository(ctx, sys); err != nil {
			return fmt.Errorf("add the CrowdSec repository: %w", err)
		}
		// The package tries to start CrowdSec on port 8080 and fails where the panel runs;
		// it is set up and started below.
		if _, err := sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", "install", "-y", "--no-install-recommends",
			"crowdsec", "crowdsec-firewall-bouncer-nftables"); err != nil {
			return fmt.Errorf("install CrowdSec: %w", err)
		}
	}
	changed, _, err := a.writeIfChanged("/etc/crowdsec/config.yaml.local", []byte("# Managed by ekPanel.\napi:\n  server:\n    listen_uri: "+crowdsecAPI+"\n"))
	if err != nil {
		return err
	}
	if err := setYAMLLine(sys, "/etc/crowdsec/local_api_credentials.yaml", "url", "http://"+crowdsecAPI); err != nil {
		return err
	}
	acq, _, err := a.writeIfChanged(crowdsecAcquis, []byte(crowdsecAcquisition))
	if err != nil {
		return err
	}
	// `cscli setup` may have added the same logs; read twice, every request would count double.
	if os.Remove(sys.path("/etc/crowdsec/acquis.d/setup.caddy.yaml")) == nil {
		acq = true
	}
	if changed || acq || fresh {
		if _, err := sys.run(ctx, "systemctl", "restart", "crowdsec"); err != nil {
			return fmt.Errorf("start CrowdSec: %w", err)
		}
	} else if !unitActive(ctx, sys, "crowdsec") {
		sys.run(ctx, "systemctl", "start", "crowdsec")
	}
	sys.run(ctx, "systemctl", "enable", "crowdsec")
	if err := waitCrowdSec(ctx, sys); err != nil {
		return err
	}
	if err := ensureCollections(ctx, sys); err != nil {
		return err
	}
	return ensureBouncer(ctx, sys)
}

// setYAMLLine replaces a top-level "key: value" line of a YAML file.
func setYAMLLine(sys *system, path, key, value string) error {
	b, err := os.ReadFile(sys.path(path))
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(l, key+":") {
			found = true
			lines[i] = key + ": " + value
		}
	}
	if !found {
		lines = append([]string{key + ": " + value}, lines...)
	}
	out := strings.Join(lines, "\n")
	if out == string(b) {
		return nil
	}
	return os.WriteFile(sys.path(path), []byte(out), 0o600)
}

func waitCrowdSec(ctx context.Context, sys *system) error {
	var err error
	for range 30 {
		if _, err = sys.run(ctx, "cscli", "lapi", "status"); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		if sys.socketWait == 0 {
			break // tests
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("CrowdSec did not start: %v", err)
}

func ensureCollections(ctx context.Context, sys *system) error {
	out, err := sys.run(ctx, "cscli", "collections", "list", "-o", "json")
	if err != nil {
		return err
	}
	var listed struct {
		Collections []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"collections"`
	}
	json.Unmarshal(out, &listed)
	enabled := map[string]bool{}
	for _, c := range listed.Collections {
		enabled[c.Name] = strings.Contains(c.Status, "enabled")
	}
	var missing []string
	for _, c := range crowdsecCollections {
		if !enabled[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := sys.run(ctx, "cscli", append([]string{"collections", "install"}, missing...)...); err != nil {
		return fmt.Errorf("install CrowdSec detections: %w", err)
	}
	_, err = sys.run(ctx, "systemctl", "reload", "crowdsec")
	return err
}

// ensureBouncer registers the firewall bouncer with the local API once, and keeps it running.
func ensureBouncer(ctx context.Context, sys *system) error {
	out, err := sys.run(ctx, "cscli", "bouncers", "list", "-o", "json")
	if err != nil {
		return err
	}
	var bouncers []struct {
		Name string `json:"name"`
	}
	json.Unmarshal(out, &bouncers)
	_, haveConfig := os.Stat(sys.path(bouncerLocal))
	registered := false
	for _, b := range bouncers {
		registered = registered || b.Name == crowdsecBouncer
	}
	if !registered || haveConfig != nil {
		if registered {
			sys.run(ctx, "cscli", "bouncers", "delete", crowdsecBouncer)
		}
		key, err := sys.run(ctx, "cscli", "bouncers", "add", crowdsecBouncer, "-o", "raw")
		if err != nil {
			return fmt.Errorf("register the firewall bouncer: %w", err)
		}
		conf := "# Managed by ekPanel.\napi_url: http://" + crowdsecAPI + "/\napi_key: " + strings.TrimSpace(string(key)) + "\n"
		if err := os.WriteFile(sys.path(bouncerLocal), []byte(conf), 0o600); err != nil {
			return err
		}
		if _, err := sys.run(ctx, "systemctl", "restart", "crowdsec-firewall-bouncer"); err != nil {
			return fmt.Errorf("start the firewall bouncer: %w", err)
		}
	} else if !unitActive(ctx, sys, "crowdsec-firewall-bouncer") {
		if _, err := sys.run(ctx, "systemctl", "restart", "crowdsec-firewall-bouncer"); err != nil {
			return fmt.Errorf("start the firewall bouncer: %w", err)
		}
	}
	_, err = sys.run(ctx, "systemctl", "enable", "crowdsec-firewall-bouncer")
	return err
}

// syncAllowlist makes CrowdSec's "ekpanel" allowlist the user's entries plus the server's
// own addresses and Cloudflare's. Allowlisted addresses are never banned, also not by the
// community blocklist.
func syncAllowlist(ctx context.Context, sys *system, user []string) error {
	want := map[string]bool{}
	for _, e := range user {
		want[e] = true
	}
	for _, e := range agentproto.CloudflareRanges {
		want[e] = true
	}
	for _, a := range globalAddresses() {
		if e := agentproto.NormalizeAllowEntry(a); e != "" {
			want[e] = true
		}
	}
	out, err := sys.run(ctx, "cscli", "allowlists", "inspect", crowdsecAllowlist, "-o", "json")
	if err != nil {
		if _, err := sys.run(ctx, "cscli", "allowlists", "create", crowdsecAllowlist, "-d", "Managed by ekPanel"); err != nil {
			return fmt.Errorf("create the allowlist: %w", err)
		}
		out = []byte("{}")
	}
	var list struct {
		Items []struct {
			Value string `json:"value"`
		} `json:"items"`
	}
	json.Unmarshal(out, &list)
	have := map[string]bool{}
	for _, it := range list.Items {
		have[it.Value] = true
	}
	var add, remove []string
	for e := range want {
		if !have[e] {
			add = append(add, e)
		}
	}
	for e := range have {
		if !want[e] {
			remove = append(remove, e)
		}
	}
	sort.Strings(add)
	sort.Strings(remove)
	if len(remove) > 0 {
		if _, err := sys.run(ctx, "cscli", append([]string{"allowlists", "remove", crowdsecAllowlist}, remove...)...); err != nil {
			return fmt.Errorf("update the allowlist: %w", err)
		}
	}
	if len(add) > 0 {
		if _, err := sys.run(ctx, "cscli", append([]string{"allowlists", "add", crowdsecAllowlist}, append(add, "-d", "ekPanel")...)...); err != nil {
			return fmt.Errorf("update the allowlist: %w", err)
		}
	}
	return nil
}

// ---- Bans and alerts ----

type csAlert struct {
	ID        int64  `json:"id"`
	Scenario  string `json:"scenario"`
	Kind      string `json:"kind"`
	Events    int    `json:"events_count"`
	CreatedAt string `json:"created_at"`
	Source    struct {
		Value string `json:"value"`
		Scope string `json:"scope"`
		CN    string `json:"cn"`
		ASN   string `json:"as_name"`
	} `json:"source"`
	Decisions []csDecision `json:"decisions"`
}

type csDecision struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Value    string `json:"value"`
	Scope    string `json:"scope"`
	Duration string `json:"duration"`
	Origin   string `json:"origin"`
	Scenario string `json:"scenario"`
}

func cscliJSON(ctx context.Context, sys *system, out any, args ...string) error {
	b, err := sys.run(ctx, "cscli", append(args, "-o", "json")...)
	if err != nil {
		return err
	}
	if s := strings.TrimSpace(string(b)); s == "" || s == "null" {
		return nil
	}
	return json.Unmarshal(b, out)
}

// listBans returns the bans made on this server (detected or by hand), not the community's.
func listBans(ctx context.Context, sys *system) ([]agentproto.Ban, error) {
	var alerts []csAlert
	if err := cscliJSON(ctx, sys, &alerts, "decisions", "list"); err != nil {
		return nil, err
	}
	bans := []agentproto.Ban{}
	now := time.Now()
	for _, a := range alerts {
		for _, d := range a.Decisions {
			if d.Type != "ban" {
				continue
			}
			left, err := time.ParseDuration(d.Duration)
			if err != nil || left <= 0 {
				continue
			}
			bans = append(bans, agentproto.Ban{ID: d.ID, IP: d.Value, Reason: d.Scenario, Origin: d.Origin, Country: a.Source.CN,
				Until: now.Add(left).Truncate(time.Second), Manually: d.Origin == "cscli"})
		}
	}
	sort.Slice(bans, func(i, j int) bool { return bans[i].Until.After(bans[j].Until) })
	return bans, nil
}

func securityBans(ctx context.Context, sys *system) (*agentproto.BansResult, *agentproto.OpError) {
	if !crowdsecInstalled(ctx, sys) || !unitActive(ctx, sys, "crowdsec") {
		return nil, opError("crowdsec_off", "CrowdSec is not running on this server")
	}
	bans, err := listBans(ctx, sys)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return &agentproto.BansResult{Bans: bans}, nil
}

func securityAlerts(ctx context.Context, sys *system) (*agentproto.AlertsResult, *agentproto.OpError) {
	if !crowdsecInstalled(ctx, sys) || !unitActive(ctx, sys, "crowdsec") {
		return nil, opError("crowdsec_off", "CrowdSec is not running on this server")
	}
	var alerts []csAlert
	if err := cscliJSON(ctx, sys, &alerts, "alerts", "list", "--limit", "100"); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	out := []agentproto.SecurityAlert{}
	for _, a := range alerts {
		if a.Kind == "cscli" || a.Scenario == "" {
			continue // bans by hand are listed with the bans
		}
		at, _ := time.Parse(time.RFC3339, a.CreatedAt)
		out = append(out, agentproto.SecurityAlert{ID: a.ID, Scenario: a.Scenario, IP: a.Source.Value, Country: a.Source.CN, AS: a.Source.ASN,
			Events: a.Events, At: at, Banned: slices.ContainsFunc(a.Decisions, func(d csDecision) bool { return d.Type == "ban" })})
	}
	return &agentproto.AlertsResult{Alerts: out}, nil
}

func securityBan(ctx context.Context, sys *system, p agentproto.BanParams) (any, *agentproto.OpError) {
	if err := p.Validate(); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "%v", err)
	}
	if !crowdsecInstalled(ctx, sys) || !unitActive(ctx, sys, "crowdsec") {
		return nil, opError("crowdsec_off", "CrowdSec is not running on this server")
	}
	if _, err := sys.run(ctx, "cscli", "decisions", "add", "--ip", p.IP, "--duration", p.Duration, "--reason", strings.TrimSpace(p.Reason)); err != nil {
		if strings.Contains(err.Error(), "allowlisted") {
			return nil, opError("allowlisted", "%s is on the allowlist", p.IP)
		}
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return securityBans(ctx, sys)
}

func securityUnban(ctx context.Context, sys *system, p agentproto.UnbanParams) (any, *agentproto.OpError) {
	if _, err := netip.ParseAddr(p.IP); err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "invalid address")
	}
	if !crowdsecInstalled(ctx, sys) || !unitActive(ctx, sys, "crowdsec") {
		return nil, opError("crowdsec_off", "CrowdSec is not running on this server")
	}
	if _, err := sys.run(ctx, "cscli", "decisions", "delete", "--ip", p.IP); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return securityBans(ctx, sys)
}

// ---- Overview ----

func securityOverview(ctx context.Context, sys *system) (*agentproto.SecurityOverview, *agentproto.OpError) {
	o := &agentproto.SecurityOverview{SSH: agentproto.SSHSettings{Ports: []int{}}}
	if out, err := sys.run(ctx, "sshd", "-T"); err == nil {
		for line := range strings.Lines(string(out)) {
			k, v, _ := strings.Cut(strings.TrimSpace(line), " ")
			switch k {
			case "port":
				if n, err := strconv.Atoi(v); err == nil {
					o.SSH.Ports = append(o.SSH.Ports, n)
				}
			case "permitrootlogin":
				o.SSH.RootLogin = v
			case "passwordauthentication":
				o.SSH.PasswordAuth = v == "yes"
			}
		}
	}
	o.Updates.ListsAge = -1
	if out, err := sys.run(ctx, "apt-get", "-s", "-o", "Debug::NoLocking=1", "upgrade"); err == nil {
		for line := range strings.Lines(string(out)) {
			if strings.HasPrefix(line, "Inst ") {
				o.Updates.Pending++
				if strings.Contains(line, "-security") {
					o.Updates.Security++
				}
			}
		}
	}
	if info, err := os.Stat(sys.path("/var/lib/apt/periodic/update-success-stamp")); err == nil {
		o.Updates.ListsAge = int(time.Since(info.ModTime()).Hours())
	}
	o.Updates.AutoUpdates = autoUpdatesOn(ctx, sys)
	_, err := os.Stat(sys.path("/run/reboot-required"))
	o.Updates.RebootRequired = err == nil
	if out, err := sys.run(ctx, "ufw", "status"); err == nil {
		o.Firewall = strings.Contains(string(out), "Status: active")
	}
	o.CrowdSec = crowdsecStatus(ctx, sys)
	return o, nil
}

const autoUpgradesConf = "/etc/apt/apt.conf.d/20auto-upgrades"

func autoUpdatesOn(ctx context.Context, sys *system) bool {
	out, _ := sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", "unattended-upgrades")
	if !strings.Contains(string(out), "install ok installed") {
		return false
	}
	b, _ := os.ReadFile(sys.path(autoUpgradesConf))
	return strings.Contains(strings.ReplaceAll(string(b), " ", ""), `APT::Periodic::Unattended-Upgrade"1"`)
}

// securityAutoUpdates turns the distribution's automatic security updates on or off.
func securityAutoUpdates(ctx context.Context, sys *system, p agentproto.AutoUpdatesParams) (any, *agentproto.OpError) {
	if p.On {
		if err := ensurePackage(ctx, sys, "unattended-upgrades"); err != nil {
			return nil, opError(agentproto.ErrFailed, "install unattended-upgrades: %v", err)
		}
	}
	v := "0"
	if p.On {
		v = "1"
	}
	conf := "// Managed by ekPanel.\nAPT::Periodic::Update-Package-Lists \"" + v + "\";\nAPT::Periodic::Unattended-Upgrade \"" + v + "\";\n"
	if err := os.WriteFile(sys.path(autoUpgradesConf), []byte(conf), 0o644); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return securityOverview(ctx, sys)
}
