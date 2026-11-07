package agentproto

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// SiteWeb is how a website answers beyond serving its files: redirects, hotlink protection
// and its PHP settings. The panel and the agent check it with the same Validate, because
// its values end up in the web server's and PHP's configuration files.
type SiteWeb struct {
	// AliasRedirect sends the site's other domains to its main domain (301).
	AliasRedirect bool       `json:"alias_redirect,omitempty"`
	Redirects     []Redirect `json:"redirects,omitempty"`
	// Hotlink, when set, refuses its file types to pages of other sites.
	Hotlink *Hotlink `json:"hotlink,omitempty"`
	// PHP holds php.ini settings named in PHPSettings.
	PHP map[string]string `json:"php,omitempty"`
	// WordPress hardening: refuse /xmlrpc.php (an old remote API used for password
	// guessing), and PHP files under wp-content/uploads (where only media belongs).
	BlockXMLRPC  bool `json:"block_xmlrpc,omitempty"`
	NoUploadsPHP bool `json:"no_uploads_php,omitempty"`
	// DocRoot is a folder inside public_html served as the website's root (such as
	// "public" for Laravel); "" serves public_html itself.
	DocRoot string `json:"doc_root,omitempty"`
}

var docRootRE = regexp.MustCompile(`^[a-z0-9_-]{1,40}(/[a-z0-9_-]{1,40}){0,2}$`)

// Redirect sends requests for a path elsewhere. From is an exact path, or a prefix ending
// in "/*"; with KeepPath, what follows the prefix is added to To.
type Redirect struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Code     int    `json:"code"`
	KeepPath bool   `json:"keep_path,omitempty"`
}

type Hotlink struct {
	Extensions []string `json:"extensions"`
	// Allowed are other host names that may embed the files ("*.example.com" for all of its
	// subdomains). The site's own domains always may.
	Allowed []string `json:"allowed,omitempty"`
}

const maxRedirects = 100

// The characters allowed in redirect paths and targets leave out spaces, quotes, braces and
// newlines, so a value is always one token of the web server's configuration.
var (
	// A path, or a prefix ending in "/*".
	redirectFromRE = regexp.MustCompile(`^(/[A-Za-z0-9._~!$&'()+,;=:@%/-]{0,500})?(/\*)?$`)
	// A path, or an http(s) address; never starting with "#", which would begin a comment.
	redirectToRE  = regexp.MustCompile(`^(https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?)?(/[A-Za-z0-9._~!$&'()+,;=:@%/?#-]{0,1000})?$`)
	extensionRE   = regexp.MustCompile(`^[a-z0-9]{1,10}$`)
	hostPatternRE = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)
)

// DefaultHotlinkExtensions are protected when hotlink protection is turned on.
var DefaultHotlinkExtensions = []string{"jpg", "jpeg", "png", "gif", "webp", "avif", "svg", "mp4", "webm", "mp3"}

// PHPSetting is one php.ini setting a site may change.
type PHPSetting struct {
	Default string
	// Admin settings are set with php_admin_value, so scripts cannot change them; others
	// with php_value, so an application may still raise them itself (as WordPress does).
	Admin bool
	Flag  bool // On or Off
	check func(string) error
}

func sizeSetting(max int) func(string) error {
	return func(v string) error {
		n, unit := v[:max0(len(v)-1)], strings.ToUpper(v[max0(len(v)-1):])
		mb, err := strconv.Atoi(n)
		if err != nil || (unit != "M" && unit != "G") {
			return errors.New("use a size such as 256M or 2G")
		}
		if unit == "G" {
			mb *= 1024
		}
		if mb < 1 || mb > max {
			return fmt.Errorf("must be between 1M and %dM", max)
		}
		return nil
	}
}

func max0(n int) int { return max(n, 0) }

func intSetting(lo, hi int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < lo || n > hi {
			return fmt.Errorf("must be a number from %d to %d", lo, hi)
		}
		return nil
	}
}

func flagSetting(v string) error {
	if v != "On" && v != "Off" {
		return errors.New("must be On or Off")
	}
	return nil
}

// PHPSettings are the php.ini settings a site may change, with their defaults.
var PHPSettings = map[string]PHPSetting{
	"memory_limit":           {Default: "256M", check: sizeSetting(8192)},
	"upload_max_filesize":    {Default: "64M", check: sizeSetting(10240)},
	"post_max_size":          {Default: "64M", check: sizeSetting(10240)},
	"max_execution_time":     {Default: "120", check: intSetting(0, 86400)},
	"max_input_time":         {Default: "120", check: intSetting(-1, 86400)},
	"max_input_vars":         {Default: "1000", check: intSetting(100, 100000)},
	"max_file_uploads":       {Default: "20", check: intSetting(1, 1000)},
	"session.gc_maxlifetime": {Default: "1440", check: intSetting(60, 2592000)},
	"display_errors":         {Default: "Off", Flag: true, check: flagSetting},
	"short_open_tag":         {Default: "Off", Flag: true, check: flagSetting},
	"allow_url_fopen":        {Default: "On", Flag: true, Admin: true, check: flagSetting},
	"date.timezone": {Default: "UTC", check: func(v string) error {
		if v == "" || strings.ContainsAny(v, " \n\"';") {
			return errors.New("not a time zone")
		}
		if _, err := time.LoadLocation(v); err != nil {
			return errors.New("not a time zone")
		}
		return nil
	}},
}

// Validate checks every value of w.
func (w SiteWeb) Validate() error {
	if w.DocRoot != "" && !docRootRE.MatchString(w.DocRoot) {
		return errors.New("the document root must be a folder inside public_html, such as public")
	}
	if len(w.Redirects) > maxRedirects {
		return fmt.Errorf("at most %d redirects", maxRedirects)
	}
	seen := map[string]bool{}
	for _, r := range w.Redirects {
		switch {
		case r.From == "" || !redirectFromRE.MatchString(r.From):
			return fmt.Errorf("redirect from %q: use a path such as /old-page or /blog/*", r.From)
		case !redirectToRE.MatchString(r.To) || r.To == "":
			return fmt.Errorf("redirect to %q: use a path such as /new-page or an address such as https://example.com/", r.To)
		case r.Code != 301 && r.Code != 302 && r.Code != 307 && r.Code != 308:
			return fmt.Errorf("redirect code %d: use 301, 302, 307 or 308", r.Code)
		case seen[r.From]:
			return fmt.Errorf("two redirects from %s", r.From)
		}
		seen[r.From] = true
	}
	if h := w.Hotlink; h != nil {
		if len(h.Extensions) == 0 || len(h.Extensions) > 40 || len(h.Allowed) > 50 {
			return errors.New("hotlink protection needs 1 to 40 file types and at most 50 allowed sites")
		}
		for _, e := range h.Extensions {
			if !extensionRE.MatchString(e) {
				return fmt.Errorf("file type %q: use letters and digits, such as jpg", e)
			}
		}
		for _, a := range h.Allowed {
			if !hostPatternRE.MatchString(a) || len(a) > 253 {
				return fmt.Errorf("allowed site %q: use a domain such as example.com or *.example.com", a)
			}
		}
	}
	for k, v := range w.PHP {
		s, ok := PHPSettings[k]
		if !ok {
			return fmt.Errorf("PHP setting %q cannot be changed", k)
		}
		if err := s.check(v); err != nil {
			return fmt.Errorf("PHP setting %s: %v", k, err)
		}
	}
	return nil
}

// PHPSettingNames lists PHPSettings in a stable order.
func PHPSettingNames() []string {
	names := make([]string, 0, len(PHPSettings))
	for k := range PHPSettings {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
