package agentproto

import (
	"errors"
	"path"
	"regexp"
	"strings"
)

// Importing a cPanel account from its backup archive (cpmove-<user>.tar.gz) kept in
// ImportDir on the server. OpImportInspect reads it and tells what is inside;
// OpImportApply puts chosen websites' files and databases' dumps into place (the panel made
// the websites and databases first).
const (
	OpImportInspect = "import.inspect"
	OpImportApply   = "import.apply"

	ImportDir = "/srv/ekpanel/imports"
)

// ImportFile names an archive in ImportDir.
func ValidImportFile(name string) error {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || len(name) > 200 ||
		!(strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz") || strings.HasSuffix(name, ".tar")) {
		return errors.New("name a .tar.gz archive in " + ImportDir)
	}
	return nil
}

type ImportInspectParams struct {
	File string `json:"file"`
}

// CPanelDomain is a website of the account: the main domain, an addon domain or a subdomain.
type CPanelDomain struct {
	Domain string `json:"domain"`
	Kind   string `json:"kind"` // main, addon, sub
	// DocRoot is the document root relative to the home folder ("public_html").
	DocRoot    string   `json:"docroot"`
	Aliases    []string `json:"aliases,omitempty"` // parked domains (main) and www
	PHPVersion string   `json:"php_version,omitempty"`
	HasCert    bool     `json:"has_cert,omitempty"`
}

type CPanelDatabase struct {
	Name  string `json:"name"`
	User  string `json:"user,omitempty"`
	Bytes int64  `json:"bytes"`
	// PasswordHash is the user's MySQL password hash (mysql_native_password), when usable.
	PasswordHash string `json:"password_hash,omitempty"`
}

type CPanelPlan struct {
	User       string           `json:"user"`
	MainDomain string           `json:"main_domain"`
	Domains    []CPanelDomain   `json:"domains"`
	Databases  []CPanelDatabase `json:"databases"`
	Cron       []string         `json:"cron"`
	HomeBytes  int64            `json:"home_bytes"`
	// Not imported yet, for the report.
	MailDomains  int      `json:"mail_domains"`
	MailAccounts int      `json:"mail_accounts"`
	DNSZones     []string `json:"dns_zones"`
	// CronSettings are lines such as MAILTO=… that cron jobs here do not take.
	CronSettings []string `json:"cron_settings,omitempty"`
}

// ImportSite: a website's files come from DocRoot (relative to the archive's home folder)
// into User's public_html.
type ImportSite struct {
	DocRoot string `json:"docroot"`
	User    string `json:"user"`
}

type ImportApplyParams struct {
	File  string       `json:"file"`
	Sites []ImportSite `json:"sites"`
	// Databases to load: the name in the archive (and here, the same).
	Databases []TransferDatabase `json:"databases"`
	// MainUser gets the files outside every document root that are worth keeping.
	MainUser string `json:"main_user,omitempty"`
	// OldUser is the cPanel account: "/home/OldUser/…" in small text files (configuration,
	// scripts) becomes the path the file landed at here.
	OldUser string `json:"old_user,omitempty"`
}

// ImportSkip counts entries of the home folder left out: a top folder by Name (mail, etc …),
// or Name "links_outside" (links out of the website) or "special_files".
type ImportSkip struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type ImportApplyResult struct {
	Files     int64        `json:"files"`
	Bytes     int64        `json:"bytes"`
	Databases []string     `json:"databases"`
	Skipped   []ImportSkip `json:"skipped,omitempty"`
	// PathsChanged are files whose old home folder path was changed (the first 50 of
	// PathsChangedCount), relative to /home.
	PathsChanged      []string `json:"paths_changed,omitempty"`
	PathsChangedCount int      `json:"paths_changed_count,omitempty"`
}

var cpanelUserRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidCPanelUser says whether name can be a cPanel account (and so a path in /home).
func ValidCPanelUser(name string) bool { return cpanelUserRE.MatchString(name) }

func (p ImportApplyParams) Validate() error {
	if err := ValidImportFile(p.File); err != nil {
		return err
	}
	for _, s := range p.Sites {
		if s.DocRoot == "" || path.IsAbs(s.DocRoot) || strings.Contains("/"+s.DocRoot+"/", "/../") || !linuxUserRE.MatchString(s.User) {
			return errors.New("invalid website")
		}
	}
	if p.MainUser != "" && !linuxUserRE.MatchString(p.MainUser) || p.OldUser != "" && !ValidCPanelUser(p.OldUser) {
		return errors.New("invalid user")
	}
	return validTransferDBs(p.Databases, true)
}
