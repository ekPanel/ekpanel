package agentproto

import "testing"

func TestSiteWebValidate(t *testing.T) {
	ok := SiteWeb{
		AliasRedirect: true,
		Redirects: []Redirect{
			{From: "/old", To: "/new", Code: 301},
			{From: "/blog/*", To: "https://blog.example.com", Code: 302, KeepPath: true},
			{From: "/*", To: "https://example.org/", Code: 308},
			{From: "/shop", To: "https://shop.example.com:8443/a?b=c#d", Code: 307},
		},
		Hotlink: &Hotlink{Extensions: DefaultHotlinkExtensions, Allowed: []string{"partner.com", "*.cdn.example.com"}},
		PHP:     map[string]string{"memory_limit": "512M", "upload_max_filesize": "2G", "display_errors": "On", "date.timezone": "Asia/Dhaka", "max_input_time": "-1"},
	}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]SiteWeb{
		"from without slash":    {Redirects: []Redirect{{From: "old", To: "/new", Code: 301}}},
		"star in the middle":    {Redirects: []Redirect{{From: "/a/*/b", To: "/new", Code: 301}}},
		"space in from":         {Redirects: []Redirect{{From: "/a b", To: "/new", Code: 301}}},
		"brace in to":           {Redirects: []Redirect{{From: "/a", To: "/{http.request.host}", Code: 301}}},
		"newline in to":         {Redirects: []Redirect{{From: "/a", To: "/x\n}\nimport /etc/passwd", Code: 301}}},
		"comment as to":         {Redirects: []Redirect{{From: "/a", To: "#x", Code: 301}}},
		"quote in to":           {Redirects: []Redirect{{From: "/a", To: `/"x`, Code: 301}}},
		"other scheme":          {Redirects: []Redirect{{From: "/a", To: "javascript:alert(1)", Code: 301}}},
		"empty to":              {Redirects: []Redirect{{From: "/a", To: "", Code: 301}}},
		"bad code":              {Redirects: []Redirect{{From: "/a", To: "/b", Code: 200}}},
		"twice the same":        {Redirects: []Redirect{{From: "/a", To: "/b", Code: 301}, {From: "/a", To: "/c", Code: 301}}},
		"no file types":         {Hotlink: &Hotlink{}},
		"bad file type":         {Hotlink: &Hotlink{Extensions: []string{"j pg"}}},
		"bad allowed":           {Hotlink: &Hotlink{Extensions: []string{"jpg"}, Allowed: []string{"evil.com|x"}}},
		"unknown php setting":   {PHP: map[string]string{"disable_functions": ""}},
		"php size too big":      {PHP: map[string]string{"memory_limit": "100G"}},
		"php size no unit":      {PHP: map[string]string{"memory_limit": "512"}},
		"php newline":           {PHP: map[string]string{"memory_limit": "1M\nphp_admin_value[open_basedir] = /"}},
		"php flag":              {PHP: map[string]string{"display_errors": "yes"}},
		"php time zone":         {PHP: map[string]string{"date.timezone": "Mars/Base"}},
		"php time zone escape":  {PHP: map[string]string{"date.timezone": "UTC\nx"}},
		"php number out of set": {PHP: map[string]string{"max_input_vars": "10"}},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: accepted %+v", name, bad)
		}
	}
}
