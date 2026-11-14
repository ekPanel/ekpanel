package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
)

// Email runs on Stalwart, one pinned version downloaded from its releases and checked
// against the published checksums. Its settings live in its own database and are changed
// through its management API, which listens on loopback only. The agent signs in with a
// random administrator password that never leaves the server (STALWART_RECOVERY_ADMIN,
// kept in a root-only environment file).

const (
	stalwartVersion = "1.0.3"
	stalwartBin     = railpackDir + "/stalwart-" + stalwartVersion
	mailConfigDir   = "/etc/ekpanel/mail"
	mailDataDir     = "/var/lib/ekpanel-mail"
	mailUser        = "ekpanel-mail"
	mailUnit        = "ekpanel-mail.service"
	mailAPIAddr     = "127.0.0.1:18900"
	mailAdminUser   = "ekpanel"
	mailStatePath   = "/var/lib/ekpanel-agent/mail.json"
	// mailReadyMarker is written once the listeners are set up, which happens in Stalwart's
	// recovery mode the first time: its default listeners use ports 443 and 8080, which
	// belong to Caddy and the panel.
	mailReadyMarker = mailDataDir + "/.ekpanel-listeners"
	mailRecoveryDir = "/run/systemd/system/" + mailUnit + ".d"
)

// stalwartSHA256 is the published digest of each release archive, per Go architecture.
var stalwartSHA256 = map[string]string{
	"amd64": "8ae481ff410cd09dd53d7f49f528dcad35d90b4b2ffb0c49c9ae23aaf43c4831",
	"arm64": "b1e49a941ac38bb5e4e5586933dc736bdf5014b1892105a98e4cdf0bd829c461",
}

var stalwartArch = map[string]string{"amd64": "x86_64", "arm64": "aarch64"}

// mailListeners are the listeners ekPanel keeps: name -> protocol, address, implicit TLS.
var mailListeners = map[string]struct {
	protocol, bind string
	implicit       bool
}{
	"smtp":        {"smtp", "[::]:25", false},
	"submissions": {"smtp", "[::]:465", true},
	"submission":  {"smtp", "[::]:587", false},
	"imaps":       {"imap", "[::]:993", true},
	"pop3s":       {"pop3", "[::]:995", true},
	"http":        {"http", mailAPIAddr, false},
}

var mailUnitTemplate = template.Must(template.New("unit").Parse(`# Managed by ekPanel. Changes here are overwritten.
[Unit]
Description=ekPanel mail server (Stalwart {{.Version}})
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
Group={{.User}}
EnvironmentFile={{.ConfigDir}}/admin.env
ExecStart={{.Bin}} --config {{.ConfigDir}}/config.json
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths={{.DataDir}}

[Install]
WantedBy=multi-user.target
`))

// mailState is what the agent remembers between applies, so that an unchanged domain
// costs a few reads and no writes. Hashes only: no secret is kept here.
type mailState struct {
	Domains   map[string]string `json:"domains"`   // domain ID -> hash of its spec
	Passwords map[string]string `json:"passwords"` // address -> hash of its password hash
	Sieve     map[string]string `json:"sieve"`     // address -> hash of its script
	Cert      string            `json:"cert"`      // hash of the loaded certificate
}

func loadMailState(sys *system) *mailState {
	st := &mailState{}
	if b, err := os.ReadFile(sys.path(mailStatePath)); err == nil {
		json.Unmarshal(b, st)
	}
	if st.Domains == nil {
		st.Domains = map[string]string{}
	}
	if st.Passwords == nil {
		st.Passwords = map[string]string{}
	}
	if st.Sieve == nil {
		st.Sieve = map[string]string{}
	}
	return st
}

func (st *mailState) save(sys *system) error {
	b, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(sys.path(mailStatePath)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(sys.path(mailStatePath), b, 0o600)
}

func hashOf(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// applyMail brings the mail server to the spec, or stops it when spec is nil.
func applyMail(ctx context.Context, sys *system, spec *agentproto.MailSpec) *agentproto.MailResult {
	if spec == nil {
		if _, err := os.Stat(sys.path(systemdDir + "/" + mailUnit)); err == nil {
			sys.run(ctx, "systemctl", "disable", "--now", mailUnit)
		}
		return nil
	}
	res := &agentproto.MailResult{OK: true}
	for _, d := range spec.Domains {
		res.Domains = append(res.Domains, agentproto.MailDomainResult{ID: d.ID, Generation: d.Generation, OK: true})
	}
	failAll := func(err error) *agentproto.MailResult {
		res.OK, res.Error = false, err.Error()
		for i := range res.Domains {
			res.Domains[i].OK, res.Domains[i].Error = false, err.Error()
		}
		return res
	}
	if err := spec.Validate(); err != nil {
		return failAll(err)
	}
	c, err := ensureStalwart(ctx, sys)
	if err != nil {
		return failAll(err)
	}
	st := loadMailState(sys)
	defer st.save(sys)
	m := &mailer{sys: sys, c: c, st: st, spec: spec}
	if err := m.load(ctx); err != nil {
		return failAll(err)
	}
	// Settings first: each domain's DNS records name the host name from them.
	if err := m.settings(ctx); err != nil {
		return failAll(fmt.Errorf("mail server settings: %w", err))
	}
	if err := m.certificate(ctx); err != nil {
		res.OK, res.Error = false, "certificate: "+err.Error()
	}
	for i, d := range spec.Domains {
		if err := d.Validate(); err != nil {
			res.Domains[i].OK, res.Domains[i].Error = false, err.Error()
			continue
		}
		var err error
		if d.Deleted {
			err = m.removeDomain(ctx, d)
		} else {
			err = m.ensureDomain(ctx, d)
		}
		if err != nil {
			res.Domains[i].OK, res.Domains[i].Error = false, err.Error()
		}
	}
	if err := m.load(ctx); err != nil {
		return failAll(err)
	}
	for i, d := range spec.Domains {
		if d.Deleted || !res.Domains[i].OK {
			continue
		}
		if err := m.fillDomain(ctx, d); err != nil {
			res.Domains[i].OK, res.Domains[i].Error = false, err.Error()
			continue
		}
		dom := m.domains[d.Domain]
		records, err := agentproto.ParseMailZone(d.Domain, dom.DNSZoneFile)
		if err != nil {
			res.Domains[i].OK, res.Domains[i].Error = false, "DNS records: "+err.Error()
			continue
		}
		res.Domains[i].DNS = records
		res.Domains[i].Usage = map[string]int64{}
		for _, mb := range d.Mailboxes {
			if a, ok := m.accounts[mb.Name+"@"+d.Domain]; ok {
				res.Domains[i].Usage[mb.ID] = a.Used
			}
		}
	}
	return res
}

// ---- Installing ----

func mailAdminPassword(sys *system) (string, error) {
	path := sys.path(mailConfigDir + "/admin.env")
	if b, err := os.ReadFile(path); err == nil {
		if v, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "STALWART_RECOVERY_ADMIN="+mailAdminUser+":"); ok && len(v) >= 32 {
			return v, nil
		}
	}
	buf := make([]byte, 24)
	rand.Read(buf)
	pw := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("STALWART_RECOVERY_ADMIN="+mailAdminUser+":"+pw+"\n"), 0o600); err != nil {
		return "", err
	}
	return pw, os.Rename(tmp, path)
}

func mailClient(sys *system) (*jmapClient, error) {
	pw, err := mailAdminPassword(sys)
	if err != nil {
		return nil, err
	}
	return &jmapClient{base: "http://" + mailAPIAddr, user: mailAdminUser, password: pw, http: &http.Client{Timeout: 60 * time.Second}}, nil
}

// ensureStalwart installs and starts the mail server, and returns a client for its API.
func ensureStalwart(ctx context.Context, sys *system) (*jmapClient, error) {
	if err := installStalwart(ctx, sys); err != nil {
		return nil, err
	}
	if _, err := sys.run(ctx, "id", "-u", mailUser); err != nil {
		if _, err := sys.run(ctx, "useradd", "--system", "--home-dir", mailDataDir, "--no-create-home", "--shell", "/usr/sbin/nologin", mailUser); err != nil {
			return nil, fmt.Errorf("create the mail server's account: %w", err)
		}
	}
	if err := os.MkdirAll(sys.path(mailDataDir), 0o700); err != nil {
		return nil, err
	}
	if _, err := sys.run(ctx, "chown", mailUser+":"+mailUser, mailDataDir); err != nil {
		return nil, err
	}
	c, err := mailClient(sys)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sys.path(mailConfigDir), 0o750); err != nil {
		return nil, err
	}
	config := []byte(`{"@type":"RocksDb","path":"` + mailDataDir + `/"}` + "\n")
	if err := os.WriteFile(sys.path(mailConfigDir+"/config.json"), config, 0o640); err != nil {
		return nil, err
	}
	if _, err := sys.run(ctx, "chown", "root:"+mailUser, mailConfigDir, mailConfigDir+"/config.json"); err != nil {
		return nil, err
	}
	var unit strings.Builder
	mailUnitTemplate.Execute(&unit, map[string]string{"Version": stalwartVersion, "User": mailUser, "ConfigDir": mailConfigDir,
		"Bin": stalwartBin, "DataDir": mailDataDir})
	a := &applier{sys: sys}
	unitChanged, _, err := a.writeIfChanged(systemdDir+"/"+mailUnit, []byte(unit.String()))
	if err != nil {
		return nil, err
	}
	if unitChanged {
		if _, err := sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(sys.path(mailReadyMarker)); err != nil {
		if err := firstMailStart(ctx, sys, c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if out, _ := sys.run(ctx, "systemctl", "is-active", mailUnit); unitChanged || strings.TrimSpace(string(out)) != "active" {
		if _, err := sys.run(ctx, "systemctl", "enable", mailUnit); err != nil {
			return nil, err
		}
		if _, err := sys.run(ctx, "systemctl", "restart", mailUnit); err != nil {
			return nil, fmt.Errorf("start the mail server: %w", err)
		}
	}
	if err := c.ready(ctx, 30*time.Second); err != nil {
		return nil, err
	}
	changed, err := fixListeners(ctx, c)
	if err != nil {
		return nil, err
	}
	if changed {
		if _, err := sys.run(ctx, "systemctl", "restart", mailUnit); err != nil {
			return nil, err
		}
		return c, c.ready(ctx, 30*time.Second)
	}
	return c, nil
}

func installStalwart(ctx context.Context, sys *system) error {
	if info, err := os.Stat(sys.path(stalwartBin)); err == nil && info.Mode().IsRegular() {
		return nil
	}
	sum, arch := stalwartSHA256[runtime.GOARCH], stalwartArch[runtime.GOARCH]
	if sum == "" {
		return fmt.Errorf("the mail server is not available for %s", runtime.GOARCH)
	}
	url := fmt.Sprintf("https://github.com/stalwartlabs/stalwart/releases/download/v%s/stalwart-%s-unknown-linux-gnu.tar.gz", stalwartVersion, arch)
	archive, err := sys.fetch(ctx, url)
	if err != nil {
		return fmt.Errorf("download the mail server: %w", err)
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != sum {
		return errors.New("the mail server download does not match its published checksum")
	}
	program, err := untarFile(archive, "stalwart")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(sys.path(stalwartBin)), 0o755); err != nil {
		return err
	}
	tmp := sys.path(stalwartBin) + ".tmp"
	if err := os.WriteFile(tmp, program, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, sys.path(stalwartBin))
}

// firstMailStart starts Stalwart in recovery mode, where it serves only its API on the
// recovery port, moves the listeners off the ports other services use, and then starts it
// for real.
func firstMailStart(ctx context.Context, sys *system, c *jmapClient) error {
	dropIn := mailRecoveryDir + "/recovery.conf"
	if err := os.MkdirAll(sys.path(mailRecoveryDir), 0o755); err != nil {
		return err
	}
	port := mailAPIAddr[strings.LastIndexByte(mailAPIAddr, ':')+1:]
	conf := "[Service]\nEnvironment=STALWART_RECOVERY_MODE=true STALWART_RECOVERY_MODE_PORT=" + port + "\n"
	if err := os.WriteFile(sys.path(dropIn), []byte(conf), 0o644); err != nil {
		return err
	}
	cleanup := func() {
		os.Remove(sys.path(dropIn))
		sys.run(ctx, "systemctl", "daemon-reload")
	}
	if _, err := sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
		cleanup()
		return err
	}
	if _, err := sys.run(ctx, "systemctl", "restart", mailUnit); err != nil {
		cleanup()
		return fmt.Errorf("start the mail server: %w", err)
	}
	if err := c.ready(ctx, 60*time.Second); err != nil {
		cleanup()
		return err
	}
	if _, err := fixListeners(ctx, c); err != nil {
		cleanup()
		return fmt.Errorf("set up the mail server's ports: %w", err)
	}
	cleanup()
	if err := os.WriteFile(sys.path(mailReadyMarker), []byte(stalwartVersion+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := sys.run(ctx, "systemctl", "enable", mailUnit); err != nil {
		return err
	}
	if _, err := sys.run(ctx, "systemctl", "restart", mailUnit); err != nil {
		return fmt.Errorf("start the mail server: %w", err)
	}
	return c.ready(ctx, 30*time.Second)
}

// fixListeners makes the listeners exactly mailListeners. Changes take effect on restart.
func fixListeners(ctx context.Context, c *jmapClient) (bool, error) {
	var have []swListener
	if err := c.get(ctx, "x:NetworkListener", map[string]any{"properties": []string{"name", "protocol", "bind", "implicitTls"}}, &have); err != nil {
		return false, err
	}
	changed := false
	seen := map[string]bool{}
	for _, l := range have {
		want, ok := mailListeners[l.Name]
		if !ok || seen[l.Name] {
			if err := c.destroy(ctx, "x:NetworkListener", l.ID); err != nil {
				return changed, err
			}
			changed = true
			continue
		}
		seen[l.Name] = true
		if l.Protocol != want.protocol || l.ImplicitTLS != want.implicit || len(l.Bind) != 1 || !l.Bind[want.bind] {
			patch := map[string]any{"protocol": want.protocol, "implicitTls": want.implicit, "bind": map[string]bool{want.bind: true}}
			if err := c.update(ctx, "x:NetworkListener", l.ID, patch); err != nil {
				return changed, err
			}
			changed = true
		}
	}
	names := make([]string, 0, len(mailListeners))
	for name := range mailListeners {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if seen[name] {
			continue
		}
		want := mailListeners[name]
		l := map[string]any{"name": name, "protocol": want.protocol, "implicitTls": want.implicit, "bind": map[string]bool{want.bind: true}}
		if _, err := c.create(ctx, "x:NetworkListener", l); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// ---- Domains, mailboxes and forwarders ----

type mailer struct {
	sys      *system
	c        *jmapClient
	st       *mailState
	spec     *agentproto.MailSpec
	domains  map[string]swDomain  // by name
	accounts map[string]swAccount // by address
	lists    map[string]swList    // by address
}

func (m *mailer) load(ctx context.Context) error {
	var domains []swDomain
	if err := m.c.get(ctx, "x:Domain", map[string]any{"properties": []string{"name", "catchAllAddress", "dnsZoneFile"}}, &domains); err != nil {
		return err
	}
	var accounts []swAccount
	if err := m.c.get(ctx, "x:Account", map[string]any{"properties": []string{"@type", "name", "domainId", "quotas", "usedDiskQuota"}}, &accounts); err != nil {
		return err
	}
	var lists []swList
	if err := m.c.get(ctx, "x:MailingList", map[string]any{"properties": []string{"name", "domainId", "recipients"}}, &lists); err != nil {
		return err
	}
	m.domains = map[string]swDomain{}
	byID := map[string]string{}
	for _, d := range domains {
		m.domains[d.Name] = d
		byID[d.ID] = d.Name
	}
	m.accounts = map[string]swAccount{}
	for _, a := range accounts {
		if a.Type == "User" && byID[a.DomainID] != "" {
			m.accounts[a.Name+"@"+byID[a.DomainID]] = a
		}
	}
	m.lists = map[string]swList{}
	for _, l := range lists {
		if byID[l.DomainID] != "" {
			m.lists[l.Name+"@"+byID[l.DomainID]] = l
		}
	}
	return nil
}

// ensureDomain creates the domain if it is missing; fillDomain does the rest once the
// server settings point at a domain.
func (m *mailer) ensureDomain(ctx context.Context, d agentproto.MailDomainSpec) error {
	if _, ok := m.domains[d.Domain]; ok {
		return nil
	}
	id, err := m.c.create(ctx, "x:Domain", map[string]any{"name": d.Domain})
	if err != nil {
		return err
	}
	m.domains[d.Domain] = swDomain{ID: id, Name: d.Domain}
	delete(m.st.Domains, d.ID)
	return nil
}

func (m *mailer) inDomain(domain string) (accounts []string, lists []string) {
	for addr := range m.accounts {
		if strings.HasSuffix(addr, "@"+domain) {
			accounts = append(accounts, addr)
		}
	}
	for addr := range m.lists {
		if strings.HasSuffix(addr, "@"+domain) {
			lists = append(lists, addr)
		}
	}
	sort.Strings(accounts)
	sort.Strings(lists)
	return
}

// fillDomain makes the domain's mailboxes, forwarders and catch-all match the spec. A
// domain whose spec and contents are unchanged since the last apply is left alone.
func (m *mailer) fillDomain(ctx context.Context, d agentproto.MailDomainSpec) error {
	dom := m.domains[d.Domain]
	accounts, lists := m.inDomain(d.Domain)
	var wantAccounts, wantLists []string
	for _, mb := range d.Mailboxes {
		wantAccounts = append(wantAccounts, mb.Name+"@"+d.Domain)
	}
	for _, f := range d.Forwarders {
		wantLists = append(wantLists, f.Name+"@"+d.Domain)
	}
	sort.Strings(wantAccounts)
	sort.Strings(wantLists)
	fingerprint := hashOf(d)
	if m.st.Domains[d.ID] == fingerprint && slices.Equal(accounts, wantAccounts) && slices.Equal(lists, wantLists) {
		return nil
	}
	delete(m.st.Domains, d.ID)

	// Addresses that move between a mailbox and a forwarder must be freed first.
	for _, addr := range lists {
		if !slices.Contains(wantLists, addr) {
			if err := m.c.destroy(ctx, "x:MailingList", m.lists[addr].ID); err != nil {
				return err
			}
			delete(m.lists, addr)
		}
	}
	for _, addr := range accounts {
		if !slices.Contains(wantAccounts, addr) {
			if err := m.c.destroy(ctx, "x:Account", m.accounts[addr].ID); err != nil {
				return err
			}
			delete(m.accounts, addr)
			delete(m.st.Passwords, addr)
			delete(m.st.Sieve, addr)
		}
	}
	for _, mb := range d.Mailboxes {
		if err := m.ensureMailbox(ctx, dom, d.Domain, mb); err != nil {
			return fmt.Errorf("%s@%s: %w", mb.Name, d.Domain, err)
		}
	}
	for _, f := range d.Forwarders {
		addr := f.Name + "@" + d.Domain
		recipients := map[string]bool{}
		for _, to := range f.To {
			recipients[strings.ToLower(to)] = true
		}
		l, ok := m.lists[addr]
		switch {
		case !ok:
			id, err := m.c.create(ctx, "x:MailingList", map[string]any{"name": f.Name, "domainId": dom.ID, "recipients": recipients})
			if err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
			m.lists[addr] = swList{ID: id, Name: f.Name, DomainID: dom.ID, Recipients: recipients}
		case !mapsEqual(l.Recipients, recipients):
			if err := m.c.update(ctx, "x:MailingList", l.ID, map[string]any{"recipients": recipients}); err != nil {
				return fmt.Errorf("%s: %w", addr, err)
			}
		}
	}
	var catchAll *string
	if d.CatchAll != "" {
		v := d.CatchAll + "@" + d.Domain
		catchAll = &v
	}
	if (catchAll == nil) != (dom.CatchAll == nil) || (catchAll != nil && *catchAll != *dom.CatchAll) {
		if err := m.c.update(ctx, "x:Domain", dom.ID, map[string]any{"catchAllAddress": catchAll}); err != nil {
			return err
		}
	}
	m.st.Domains[d.ID] = fingerprint
	return nil
}

func mapsEqual(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func (m *mailer) ensureMailbox(ctx context.Context, dom swDomain, domain string, mb agentproto.MailboxSpec) error {
	addr := mb.Name + "@" + domain
	quotas := map[string]int64{}
	if mb.QuotaMB > 0 {
		quotas["maxDiskQuota"] = int64(mb.QuotaMB) << 20
	}
	secret, pwHash := mb.PasswordHash, hashOf(mb.PasswordHash)
	if mb.Locked {
		// A hash of a random password nobody knows: signing in fails, mail still arrives.
		// The stored marker keeps it from being replaced on every apply.
		pwHash = "locked"
		if m.st.Passwords[addr] != pwHash {
			h, err := auth.MailPasswordHash(auth.NewToken())
			if err != nil {
				return err
			}
			secret = h
		}
	}
	credentials := map[string]any{"0": map[string]any{"@type": "Password", "secret": secret}}
	a, ok := m.accounts[addr]
	if !ok {
		id, err := m.c.create(ctx, "x:Account", map[string]any{"@type": "User", "name": mb.Name, "domainId": dom.ID,
			"credentials": credentials, "quotas": quotas})
		if err != nil {
			return err
		}
		a = swAccount{ID: id, Type: "User", Name: mb.Name, DomainID: dom.ID, Quotas: quotas}
		m.accounts[addr] = a
		m.st.Passwords[addr] = pwHash
		delete(m.st.Sieve, addr)
	} else {
		patch := map[string]any{}
		if a.Quotas["maxDiskQuota"] != quotas["maxDiskQuota"] {
			patch["quotas"] = quotas
		}
		if m.st.Passwords[addr] != pwHash {
			patch["credentials"] = credentials
		}
		if len(patch) > 0 {
			if err := m.c.update(ctx, "x:Account", a.ID, patch); err != nil {
				return err
			}
			m.st.Passwords[addr] = pwHash
		}
	}
	script := sieveScript(mb)
	if m.st.Sieve[addr] == hashOf(script) {
		return nil
	}
	if err := m.setSieve(ctx, a.ID, script); err != nil {
		return fmt.Errorf("forwarding and auto-reply: %w", err)
	}
	m.st.Sieve[addr] = hashOf(script)
	return nil
}

const sieveName = "ekpanel"

// setSieve installs and activates the account's ekPanel script, or removes it when script
// is empty. Mail apps cannot change scripts: ManageSieve is not offered.
func (m *mailer) setSieve(ctx context.Context, accountID, script string) error {
	var scripts []swSieve
	if err := m.c.get(ctx, "SieveScript", map[string]any{"accountId": accountID}, &scripts); err != nil {
		return err
	}
	var ours *swSieve
	var others []string
	for i, s := range scripts {
		if s.Name == sieveName {
			ours = &scripts[i]
		} else {
			others = append(others, s.ID)
		}
	}
	if script == "" {
		if ours == nil {
			return nil
		}
		_, err := m.c.set(ctx, "SieveScript", map[string]any{"accountId": accountID, "onSuccessDeactivateScript": true, "destroy": []string{ours.ID}})
		return err
	}
	blob, err := m.c.upload(ctx, accountID, "application/sieve", []byte(script))
	if err != nil {
		return err
	}
	args := map[string]any{"accountId": accountID}
	if ours == nil {
		args["create"] = map[string]any{"s": map[string]any{"name": sieveName, "blobId": blob}}
		args["onSuccessActivateScript"] = "#s"
	} else {
		args["update"] = map[string]any{ours.ID: map[string]any{"blobId": blob}}
		args["onSuccessActivateScript"] = ours.ID
	}
	if _, err := m.c.set(ctx, "SieveScript", args); err != nil {
		return err
	}
	// Scripts made outside ekPanel (such as Stalwart's own vacation script) would compete
	// for the one active slot.
	if len(others) > 0 {
		_, err = m.c.set(ctx, "SieveScript", map[string]any{"accountId": accountID, "destroy": others})
	}
	return err
}

// sieveQuote writes s as a Sieve quoted string.
func sieveQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// sieveScript is the filter for a mailbox's auto-reply and forwarding, or "" if it has
// neither.
func sieveScript(mb agentproto.MailboxSpec) string {
	if mb.AutoReply == nil && len(mb.ForwardTo) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Managed by ekPanel. Changes here are overwritten.\n")
	b.WriteString(`require ["vacation", "copy", "date", "relational"];` + "\n")
	if r := mb.AutoReply; r != nil {
		var conds []string
		if r.Start != "" {
			conds = append(conds, `currentdate :value "ge" "date" `+sieveQuote(r.Start))
		}
		if r.End != "" {
			conds = append(conds, `currentdate :value "le" "date" `+sieveQuote(r.End))
		}
		reply := "vacation :days 1 :subject " + sieveQuote(r.Subject) + " " + sieveQuote(r.Body) + ";\n"
		if len(conds) > 0 {
			b.WriteString("if allof(" + strings.Join(conds, ", ") + ") {\n  " + reply + "}\n")
		} else {
			b.WriteString(reply)
		}
	}
	for i, to := range mb.ForwardTo {
		// Without a copy kept, the last forward takes the message instead of copying it.
		if !mb.KeepCopy && i == len(mb.ForwardTo)-1 {
			b.WriteString("redirect " + sieveQuote(to) + ";\n")
		} else {
			b.WriteString("redirect :copy " + sieveQuote(to) + ";\n")
		}
	}
	return b.String()
}

// removeDomain deletes a domain with all its mail. Stalwart refuses to delete a domain
// that still has accounts, lists or DKIM keys.
func (m *mailer) removeDomain(ctx context.Context, d agentproto.MailDomainSpec) error {
	defer delete(m.st.Domains, d.ID)
	dom, ok := m.domains[d.Domain]
	if !ok {
		return nil
	}
	accounts, lists := m.inDomain(d.Domain)
	for _, addr := range lists {
		if err := m.c.destroy(ctx, "x:MailingList", m.lists[addr].ID); err != nil {
			return err
		}
		delete(m.lists, addr)
	}
	for _, addr := range accounts {
		if err := m.c.destroy(ctx, "x:Account", m.accounts[addr].ID); err != nil {
			return err
		}
		delete(m.accounts, addr)
		delete(m.st.Passwords, addr)
		delete(m.st.Sieve, addr)
	}
	var keys []swDkim
	if err := m.c.get(ctx, "x:DkimSignature", map[string]any{"properties": []string{"domainId"}}, &keys); err != nil {
		return err
	}
	var ids []string
	for _, k := range keys {
		if k.DomainID == dom.ID {
			ids = append(ids, k.ID)
		}
	}
	if err := m.c.destroy(ctx, "x:DkimSignature", ids...); err != nil {
		return err
	}
	if err := m.c.destroy(ctx, "x:Domain", dom.ID); err != nil {
		return err
	}
	delete(m.domains, d.Domain)
	return nil
}

// settings sets the server's host name (its SMTP greeting and every domain's MX target).
// Stalwart also needs a default domain, which can never be deleted: that is the host name
// itself, so that every mail domain of the panel can come and go.
func (m *mailer) settings(ctx context.Context) error {
	host, ok := m.domains[m.spec.Hostname]
	if !ok {
		id, err := m.c.create(ctx, "x:Domain", map[string]any{"name": m.spec.Hostname})
		if err != nil {
			return err
		}
		host = swDomain{ID: id, Name: m.spec.Hostname}
		m.domains[host.Name] = host
	}
	var set []swSettings
	if err := m.c.get(ctx, "x:SystemSettings", map[string]any{"ids": []string{"singleton"}}, &set); err != nil {
		return err
	}
	if len(set) != 1 {
		return errors.New("no settings")
	}
	patch := map[string]any{}
	if set[0].DefaultHostname != m.spec.Hostname {
		patch["defaultHostname"] = m.spec.Hostname
	}
	if set[0].DefaultDomainID != host.ID {
		patch["defaultDomainId"] = host.ID
	}
	if len(patch) == 0 {
		return nil
	}
	if err := m.c.update(ctx, "x:SystemSettings", "singleton", patch); err != nil {
		return err
	}
	return m.c.action(ctx, "ReloadSettings")
}

// caddyCertDir is where Caddy keeps the certificates it obtains.
const caddyCertDir = "/var/lib/caddy/.local/share/caddy/certificates"

// certificate gives the mail server the certificate Caddy obtained for the mail host name
// (Caddy serves webmail there), once it exists and whenever Caddy renews it.
func (m *mailer) certificate(ctx context.Context) error {
	host := m.spec.Hostname
	matches, _ := filepath.Glob(m.sys.path(caddyCertDir + "/*/" + host + "/" + host + ".crt"))
	var newest string
	var newestTime time.Time
	for _, p := range matches {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(newestTime) {
			newest, newestTime = p, info.ModTime()
		}
	}
	if newest == "" {
		return nil // Caddy has not obtained it yet; a later apply loads it
	}
	cert, err := os.ReadFile(newest)
	if err != nil {
		return err
	}
	key, err := os.ReadFile(strings.TrimSuffix(newest, ".crt") + ".key")
	if err != nil {
		return err
	}
	sum := hashOf(string(cert))
	if m.st.Cert == sum {
		return nil
	}
	var have []struct {
		ID string `json:"id"`
	}
	if err := m.c.get(ctx, "x:Certificate", map[string]any{"properties": []string{"issuer"}}, &have); err != nil {
		return err
	}
	id, err := m.c.create(ctx, "x:Certificate", map[string]any{
		"certificate": map[string]any{"@type": "Text", "value": string(cert)},
		"privateKey":  map[string]any{"@type": "Text", "secret": string(key)},
	})
	if err != nil {
		return err
	}
	if err := m.c.update(ctx, "x:SystemSettings", "singleton", map[string]any{"defaultCertificateId": id}); err != nil {
		return err
	}
	var old []string
	for _, h := range have {
		old = append(old, h.ID)
	}
	if err := m.c.destroy(ctx, "x:Certificate", old...); err != nil {
		return err
	}
	if err := m.c.action(ctx, "ReloadTlsCertificates"); err != nil {
		return err
	}
	m.st.Cert = sum
	return nil
}
