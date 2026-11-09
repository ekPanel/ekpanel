package agentproto

import (
	"errors"
	"regexp"
	"slices"
)

// ---- WordPress toolkit ----
//
// The agent runs WP-CLI as the site's account for everything inside a WordPress site.
// Database copies (staging) run as root with the MariaDB tools, between databases named in
// the sites' own wp-config.php files.

const (
	// OpWordPressInfo reads a site's WordPress: versions, available updates, plugins,
	// themes, maintenance mode and hardening. A site without WordPress answers Installed false.
	OpWordPressInfo = "wordpress.info"
	// OpWordPressUpdate updates core, plugins and themes, checking the site still answers
	// after each one and putting the old files back if it does not.
	OpWordPressUpdate = "wordpress.update"
	// OpWordPressManage changes one thing: maintenance mode, a plugin on or off, file editing
	// from the dashboard, or file permissions.
	OpWordPressManage = "wordpress.manage"
	// OpWordPressClone copies one WordPress site's files and/or database over another's on
	// the same server, as for making a staging copy or pushing it back.
	OpWordPressClone = "wordpress.clone"
)

// WordPressSite is where a WordPress site lives on the server.
type WordPressSite struct {
	User       string `json:"user"`
	Domain     string `json:"domain"`
	PHPVersion string `json:"php_version"`
}

type WordPressInfoParams struct {
	Site WordPressSite `json:"site"`
}

type WordPressInfo struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// CoreUpdate is the newest version available, CoreMinor the newest of the installed
	// branch (security and maintenance releases); empty when current.
	CoreUpdate string          `json:"core_update,omitempty"`
	CoreMinor  string          `json:"core_minor,omitempty"`
	Plugins    []WordPressItem `json:"plugins"`
	Themes     []WordPressItem `json:"themes"`
	// Maintenance shows visitors a "briefly unavailable" page.
	Maintenance bool `json:"maintenance"`
	// FileEditDisabled is DISALLOW_FILE_EDIT: no editing plugin and theme code from the dashboard.
	FileEditDisabled bool `json:"file_edit_disabled"`
	Debug            bool `json:"debug"`
	// LoosePermissions counts files and folders that others than the site's account may write.
	LoosePermissions int    `json:"loose_permissions"`
	SiteURL          string `json:"site_url,omitempty"`
	Error            string `json:"error,omitempty"` // WordPress is there but WP-CLI could not read it
}

// WordPressItem is a plugin or theme.
type WordPressItem struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Status  string `json:"status"` // active, inactive, must-use, dropin, parent …
	Version string `json:"version"`
	Update  string `json:"update,omitempty"` // version available
}

// WordPressUpdateParams says what to update. Core is "", "minor" (newest of the installed
// branch) or "latest"; Plugins and Themes are names, or ["*"] for every one with an update.
type WordPressUpdateParams struct {
	Site    WordPressSite `json:"site"`
	Core    string        `json:"core,omitempty"`
	Plugins []string      `json:"plugins,omitempty"`
	Themes  []string      `json:"themes,omitempty"`
}

type WordPressUpdateResult struct {
	Items []WordPressUpdated `json:"items"`
	Info  WordPressInfo      `json:"info"`
}

// WordPressUpdated is what happened to one update. Outcome is "updated", "rolled_back" (the
// site broke and the old files are back), "failed" or "current".
type WordPressUpdated struct {
	Kind    string `json:"kind"` // core, plugin, theme
	Name    string `json:"name"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	Outcome string `json:"outcome"`
	Error   string `json:"error,omitempty"`
}

type WordPressManageParams struct {
	Site   WordPressSite `json:"site"`
	Action string        `json:"action"` // see WordPressActions
	Name   string        `json:"name,omitempty"`
}

// WordPressActions are the changes OpWordPressManage makes. Plugin actions need Name.
var WordPressActions = []string{"maintenance_on", "maintenance_off", "plugin_activate", "plugin_deactivate",
	"file_edit_off", "file_edit_on", "fix_permissions"}

// WordPressCloneParams copies From over To. With Database, To's database gets From's tables
// (To's own are dropped first) and From's addresses are replaced by To's. When To has no
// WordPress yet (a new staging copy), its wp-config.php is From's with ToDB's credentials;
// otherwise To keeps its own. Before overwriting a site that already has WordPress, the
// agent saves its database to its tmp folder and keeps replaced files aside.
type WordPressCloneParams struct {
	From     WordPressSite `json:"from"`
	To       WordPressSite `json:"to"`
	Files    bool          `json:"files"`
	Database bool          `json:"database"`
	ToDB     *CloneDB      `json:"to_db,omitempty"`
	// NoIndex asks search engines not to index To (for staging copies).
	NoIndex bool `json:"no_index,omitempty"`
}

type CloneDB struct {
	Name     string `json:"name"`
	User     string `json:"user"`
	Password string `json:"password"`
}

type WordPressCloneResult struct {
	// Backup is where the overwritten site's database was saved, if anywhere.
	Backup string        `json:"backup,omitempty"`
	Info   WordPressInfo `json:"info"`
}

var (
	wpSlugRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	wpUserRE = regexp.MustCompile(`^[a-z][a-z0-9_]{2,31}$`)
	wpDBRE   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	wpPassRE = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
)

// ValidWordPressSlug reports whether s may be a plugin or theme name.
func ValidWordPressSlug(s string) bool {
	return wpSlugRE.MatchString(s) && !slices.Contains([]string{".", ".."}, s)
}

func (s WordPressSite) Validate() error {
	switch {
	case !wpUserRE.MatchString(s.User):
		return errors.New("invalid site account")
	case !ValidMailHost(s.Domain):
		return errors.New("invalid domain")
	case !slices.Contains(PHPVersions, s.PHPVersion):
		return errors.New("invalid PHP version")
	}
	return nil
}

func (p WordPressUpdateParams) Validate() error {
	if err := p.Site.Validate(); err != nil {
		return err
	}
	if p.Core != "" && p.Core != "minor" && p.Core != "latest" {
		return errors.New("invalid core update")
	}
	for _, list := range [][]string{p.Plugins, p.Themes} {
		for _, n := range list {
			if n != "*" && !ValidWordPressSlug(n) {
				return errors.New("invalid plugin or theme name")
			}
		}
	}
	return nil
}

func (p WordPressManageParams) Validate() error {
	if err := p.Site.Validate(); err != nil {
		return err
	}
	if !slices.Contains(WordPressActions, p.Action) {
		return errors.New("unknown action")
	}
	if (p.Action == "plugin_activate" || p.Action == "plugin_deactivate") && !ValidWordPressSlug(p.Name) {
		return errors.New("invalid plugin name")
	}
	return nil
}

func (p WordPressCloneParams) Validate() error {
	if err := p.From.Validate(); err != nil {
		return err
	}
	if err := p.To.Validate(); err != nil {
		return err
	}
	if p.From.User == p.To.User {
		return errors.New("a site cannot be copied onto itself")
	}
	if !p.Files && !p.Database {
		return errors.New("nothing to copy")
	}
	if p.ToDB != nil && (!wpDBRE.MatchString(p.ToDB.Name) || !wpDBRE.MatchString(p.ToDB.User) || !wpPassRE.MatchString(p.ToDB.Password)) {
		return errors.New("invalid database")
	}
	return nil
}
