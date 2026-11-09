package agentproto

import (
	"errors"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---- PHP application installer ----
//
// Besides WordPress, ekPanel installs these PHP applications into an empty website, each
// with a MariaDB database of its own. Joomla and Moodle come as release archives checked
// against their published SHA-256; Drupal and Laravel are made with Composer (a pinned,
// checked composer.phar), which takes their packages from Packagist.

const OpPHPAppInstall = "phpapp.install"

// PHPApp describes one application the installer offers.
type PHPApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"` // what is installed: an exact release or a major version
	MinPHP  string `json:"min_php"`
	// DocRoot is the folder inside public_html the web server serves ("" for public_html).
	DocRoot string `json:"doc_root,omitempty"`
	// Admin is false for frameworks that have no admin account (Laravel).
	Admin     bool   `json:"admin"`
	AdminPath string `json:"admin_path,omitempty"`
	// PHP settings the application needs, set on the site before installing.
	PHP     map[string]string `json:"php,omitempty"`
	Website string            `json:"website"`
}

var PHPApps = []PHPApp{
	{ID: "joomla", Name: "Joomla", Version: "6.2.0", MinPHP: "8.3", Admin: true, AdminPath: "/administrator/", Website: "https://www.joomla.org"},
	{ID: "drupal", Name: "Drupal", Version: "11", MinPHP: "8.3", DocRoot: "web", Admin: true, AdminPath: "/user/login", Website: "https://www.drupal.org"},
	{ID: "moodle", Name: "Moodle", Version: "5.2.4", MinPHP: "8.3", DocRoot: "public", Admin: true, AdminPath: "/login/index.php",
		PHP: map[string]string{"max_input_vars": "5000"}, Website: "https://moodle.org"},
	{ID: "laravel", Name: "Laravel", Version: "13", MinPHP: "8.3", DocRoot: "public", Website: "https://laravel.com"},
}

// FindPHPApp returns the application with the ID.
func FindPHPApp(id string) (PHPApp, bool) {
	i := slices.IndexFunc(PHPApps, func(a PHPApp) bool { return a.ID == id })
	if i < 0 {
		return PHPApp{}, false
	}
	return PHPApps[i], true
}

// PHPAtLeast reports whether PHP version v (such as "8.4") is min or newer.
func PHPAtLeast(v, min string) bool {
	parse := func(s string) (int, int) {
		maj, minor, _ := strings.Cut(s, ".")
		a, b := 0, 0
		for _, c := range maj {
			a = a*10 + int(c-'0')
		}
		for _, c := range minor {
			b = b*10 + int(c-'0')
		}
		return a, b
	}
	a1, b1 := parse(v)
	a2, b2 := parse(min)
	return a1 > a2 || (a1 == a2 && b1 >= b2)
}

type PHPAppInstallParams struct {
	App        string  `json:"app"`
	User       string  `json:"user"`
	Domain     string  `json:"domain"`
	PHPVersion string  `json:"php_version"`
	DB         CloneDB `json:"db"`
	Title      string  `json:"title"`
	AdminUser  string  `json:"admin_user,omitempty"`
	// AdminPassword reaches the installers through the environment, never as an argument.
	AdminPassword string `json:"admin_password,omitempty"`
	AdminEmail    string `json:"admin_email,omitempty"`
}

type PHPAppInstallResult struct {
	Version string `json:"version"`
}

var appAdminRE = regexp.MustCompile(`^[A-Za-z0-9._@-]{3,60}$`)

func (p PHPAppInstallParams) Validate() error {
	app, ok := FindPHPApp(p.App)
	switch {
	case !ok:
		return errors.New("unknown application")
	case !wpUserRE.MatchString(p.User):
		return errors.New("invalid site account")
	case !ValidMailHost(p.Domain):
		return errors.New("invalid domain")
	case !slices.Contains(PHPVersions, p.PHPVersion) || !PHPAtLeast(p.PHPVersion, app.MinPHP):
		return errors.New(app.Name + " needs PHP " + app.MinPHP + " or newer")
	case !wpDBRE.MatchString(p.DB.Name) || !wpDBRE.MatchString(p.DB.User) || !wpPassRE.MatchString(p.DB.Password):
		return errors.New("invalid database")
	case p.Title == "" || utf8.RuneCountInString(p.Title) > 100 || strings.IndexFunc(p.Title, unicode.IsControl) >= 0:
		return errors.New("the site title must be 1–100 characters on one line")
	}
	if !app.Admin {
		return nil
	}
	if !appAdminRE.MatchString(p.AdminUser) {
		return errors.New("the admin user name may use letters, digits and . _ @ - (3–60 characters)")
	}
	if !wpPassRE.MatchString(p.AdminPassword) {
		return errors.New("invalid generated password")
	}
	if a, err := mail.ParseAddress(p.AdminEmail); err != nil || a.Address != p.AdminEmail {
		return errors.New("invalid admin email address")
	}
	return nil
}
