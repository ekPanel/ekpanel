package catalog

import (
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var (
	varRefRE  = regexp.MustCompile(`(?:^|[^$])\$\{([A-Za-z_][A-Za-z0-9_]*)`)
	imageRE   = regexp.MustCompile(`(?m)^\s+image:\s*(\S+)\s*$`)
	idRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	volumeRE  = regexp.MustCompile(`(?m)^\s+-\s+([^:\s]+):/`)
	forbidden = []string{"privileged", "cap_add", "devices:", "network_mode", "pid:", "ipc:", "ports:", "expose:", "docker.sock",
		"security_opt", "userns_mode", "cgroup_parent", "sysctls", "volumes_from", "env_file", "build:", "extends:", "include:"}
	categories = []string{"monitoring", "automation", "development", "security", "analytics", "publishing", "productivity", "notifications", "database"}
)

func TestTemplatesFollowTheRules(t *testing.T) {
	seen := map[string]bool{}
	for _, tpl := range All() {
		t.Run(tpl.ID, func(t *testing.T) {
			if !idRE.MatchString(tpl.ID) || seen[tpl.ID] {
				t.Fatalf("the ID must be a valid, unique app name")
			}
			seen[tpl.ID] = true
			if tpl.Name == "" || tpl.Version < 1 || tpl.MemoryMB <= 0 || !strings.HasPrefix(tpl.Website, "https://") || !slices.Contains(categories, tpl.Category) {
				t.Errorf("name, version, memory, website or category missing: %+v", tpl)
			}
			if tpl.NeedsDomain && tpl.Port == 0 {
				t.Error("a template that needs a domain needs a port")
			}
			c := tpl.Compose
			if !strings.Contains(c, "\n  "+tpl.Service+":\n") {
				t.Errorf("the Compose file has no service %q", tpl.Service)
			}
			for _, word := range forbidden {
				if strings.Contains(c, word) {
					t.Errorf("the Compose file uses %q", word)
				}
			}
			for _, m := range volumeRE.FindAllStringSubmatch(c, -1) {
				if strings.ContainsAny(m[1][:1], "/.~$") {
					t.Errorf("volume %q is a folder of the host; use a named volume", m[1])
				}
			}
			// Every image is declared, so packages can allow or refuse the template.
			var images []string
			for _, m := range imageRE.FindAllStringSubmatch(c, -1) {
				images = append(images, m[1])
			}
			if !slices.Equal(slices.Sorted(slices.Values(images)), slices.Sorted(slices.Values(tpl.Images))) {
				t.Errorf("images in the file %v differ from the declared %v", images, tpl.Images)
			}
			// Every ${VAR} is declared, and every declared variable is used.
			used := map[string]bool{}
			for _, m := range varRefRE.FindAllStringSubmatch(c, -1) {
				used[m[1]] = true
			}
			for _, v := range tpl.Vars {
				if (v.Generate == "") == (v.Value == "") {
					t.Errorf("%s: set exactly one of Generate and Value", v.Name)
				}
				if v.Generate != "" && v.Generate != "password" && v.Generate != "hex" {
					t.Errorf("%s: unknown generator %q", v.Name, v.Generate)
				}
				if !used[v.Name] {
					t.Errorf("%s is declared but not used", v.Name)
				}
				delete(used, v.Name)
			}
			for name := range used {
				t.Errorf("${%s} is used but not declared", name)
			}
		})
	}
}

// The web UI describes each template; the Bangla file is checked against English by the
// UI's own translation check.
func TestTemplatesAreDescribed(t *testing.T) {
	b, err := os.ReadFile("../../web/src/i18n/locales/en.json")
	if err != nil {
		t.Fatal(err)
	}
	var en struct {
		Catalog struct {
			Apps       map[string]struct{ Description string } `json:"apps"`
			Categories map[string]string                       `json:"categories"`
		} `json:"catalog"`
	}
	if err := json.Unmarshal(b, &en); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range All() {
		if en.Catalog.Apps[tpl.ID].Description == "" {
			t.Errorf("catalog.apps.%s.description is missing from en.json", tpl.ID)
		}
	}
	for _, c := range categories {
		if en.Catalog.Categories[c] == "" {
			t.Errorf("catalog.categories.%s is missing from en.json", c)
		}
	}
}

func TestEnv(t *testing.T) {
	tpl, ok := Get("nextcloud")
	if !ok {
		t.Fatal("nextcloud is missing")
	}
	a, b := tpl.Env("cloud.example.com"), tpl.Env("cloud.example.com")
	get := func(env []Variable, k string) string {
		for _, e := range env {
			if e.Key == k {
				return e.Value
			}
		}
		t.Fatalf("no %s", k)
		return ""
	}
	if get(a, "DOMAIN") != "cloud.example.com" || get(a, "ADMIN_USER") != "admin" {
		t.Errorf("fixed values: %v", a)
	}
	pw := get(a, "ADMIN_PASSWORD")
	if len(pw) != 24 || strings.Trim(pw, passwordChars) != "" || pw == get(b, "ADMIN_PASSWORD") || pw == get(a, "DB_PASSWORD") {
		t.Errorf("passwords must be fresh, 24 safe characters: %q", pw)
	}
	n8n, _ := Get("n8n")
	key := get(n8n.Env("x.test"), "N8N_ENCRYPTION_KEY")
	if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" {
		t.Errorf("hex key: %q", key)
	}

	tpl.FollowDomain(a, "files.example.org")
	if get(a, "DOMAIN") != "files.example.org" || get(a, "ADMIN_PASSWORD") != pw {
		t.Errorf("only values made from the domain follow it: %v", a)
	}
	if _, ok := Get("nope"); ok {
		t.Error("unknown template found")
	}
}
