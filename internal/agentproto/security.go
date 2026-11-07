package agentproto

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"
)

// ---- Security ----
//
// CrowdSec reads the server's SSH and web server logs, bans addresses that attack them in
// the firewall (nftables, next to ufw), and shares and receives attacker addresses with
// its community. The panel sends whether it runs and the addresses never to ban; bans are
// listed, added and lifted with their own operations.

// SecuritySpec is part of the desired state of every server.
type SecuritySpec struct {
	CrowdSec bool `json:"crowdsec"`
	// Allowlist is addresses and networks never banned, besides those the agent always
	// adds: the server's own addresses and Cloudflare's.
	Allowlist []string `json:"allowlist"`
}

type SecurityResult struct {
	CrowdSec CrowdSecStatus `json:"crowdsec"`
	Error    string         `json:"error,omitempty"`
}

type CrowdSecStatus struct {
	Installed     bool   `json:"installed"`
	Active        bool   `json:"active"`
	BouncerActive bool   `json:"bouncer_active"`
	Version       string `json:"version,omitempty"`
	Bans          int    `json:"bans"`
}

const (
	OpSecurityOverview    = "security.overview"
	OpSecurityBans        = "security.bans"
	OpSecurityAlerts      = "security.alerts"
	OpSecurityBan         = "security.ban"
	OpSecurityUnban       = "security.unban"
	OpSecurityAutoUpdates = "security.auto_updates"
)

// SecurityOverview is what an administrator should look at on a server.
type SecurityOverview struct {
	SSH      SSHSettings    `json:"ssh"`
	Updates  UpdateStatus   `json:"updates"`
	Firewall bool           `json:"firewall"` // ufw active
	CrowdSec CrowdSecStatus `json:"crowdsec"`
}

type SSHSettings struct {
	Ports        []int  `json:"ports"`
	RootLogin    string `json:"root_login"` // sshd's PermitRootLogin: yes, no, prohibit-password …
	PasswordAuth bool   `json:"password_auth"`
}

type UpdateStatus struct {
	Pending        int  `json:"pending"`
	Security       int  `json:"security"`
	AutoUpdates    bool `json:"auto_updates"`
	RebootRequired bool `json:"reboot_required"`
	// ListsAge is how old the package lists are, in hours; -1 when unknown.
	ListsAge int `json:"lists_age"`
}

// Ban is an address the firewall drops.
type Ban struct {
	ID       int64     `json:"id"`
	IP       string    `json:"ip"`
	Reason   string    `json:"reason"` // CrowdSec's scenario, or the reason given by hand
	Origin   string    `json:"origin"` // "crowdsec" (detected), "cscli" (by hand) …
	Country  string    `json:"country,omitempty"`
	Until    time.Time `json:"until"`
	Manually bool      `json:"manually"`
}

// SecurityAlert is an attack CrowdSec detected.
type SecurityAlert struct {
	ID       int64     `json:"id"`
	Scenario string    `json:"scenario"`
	IP       string    `json:"ip"`
	Country  string    `json:"country,omitempty"`
	AS       string    `json:"as,omitempty"`
	Events   int       `json:"events"`
	At       time.Time `json:"at"`
	Banned   bool      `json:"banned"`
}

type BansResult struct {
	Bans []Ban `json:"bans"`
}

type AlertsResult struct {
	Alerts []SecurityAlert `json:"alerts"`
}

// BanDurations are the lengths a ban by hand may have.
var BanDurations = []string{"1h", "4h", "24h", "168h", "720h", "8760h"}

type BanParams struct {
	IP       string `json:"ip"`
	Duration string `json:"duration"`
	Reason   string `json:"reason"`
}

type UnbanParams struct {
	IP string `json:"ip"`
}

type AutoUpdatesParams struct {
	On bool `json:"on"`
}

// ValidBanIP reports whether s is one address that may be banned: not a network (the
// firewall bouncer stores single addresses), and not private, loopback or the like.
func ValidBanIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.String() == s && a.Zone() == "" && a.IsGlobalUnicast() && !a.IsPrivate()
}

func (p BanParams) Validate() error {
	switch {
	case !ValidBanIP(p.IP):
		return errors.New("enter one public IP address")
	case !slices.Contains(BanDurations, p.Duration):
		return errors.New("unknown duration")
	case strings.TrimSpace(p.Reason) == "" || len(p.Reason) > 100 || strings.IndexFunc(p.Reason, unicode.IsControl) >= 0:
		return errors.New("the reason must be one line of up to 100 characters")
	}
	return nil
}

// MaxAllowlist is how many entries a server's allowlist may have.
const MaxAllowlist = 200

// NormalizeAllowEntry returns an address or network in canonical form, or "" if s is neither.
func NormalizeAllowEntry(s string) string {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil && p.Addr().Zone() == "" {
		if p.IsSingleIP() {
			return p.Addr().String()
		}
		return p.Masked().String()
	}
	if a, err := netip.ParseAddr(s); err == nil && a.Zone() == "" {
		return a.String()
	}
	return ""
}

func (s SecuritySpec) Validate() error {
	if len(s.Allowlist) > MaxAllowlist {
		return errors.New("too many allowlist entries")
	}
	for _, e := range s.Allowlist {
		if NormalizeAllowEntry(e) != e {
			return errors.New("invalid allowlist entry " + e)
		}
	}
	return nil
}

// CloudflareRanges are Cloudflare's published addresses
// (https://www.cloudflare.com/ips/). Requests through Cloudflare come from them, so they
// are never banned; the visitor's own address is what gets banned.
var CloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18", "108.162.192.0/18",
	"190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22", "2400:cb00::/32", "2606:4700::/32", "2803:f800::/32",
	"2405:b500::/32", "2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
}
