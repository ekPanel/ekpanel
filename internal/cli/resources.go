package cli

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// stringList is a flag that can be given more than once.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

const defaultWaitTimeout = 15 * time.Minute

func waitFlags(fs *flag.FlagSet, what string) (wait *bool, timeout *time.Duration) {
	wait = fs.Bool("wait", false, "wait until "+what)
	timeout = fs.Duration("timeout", defaultWaitTimeout, "with --wait, stop waiting after this `duration`")
	return
}

// ---- Servers ----

type serverInfo struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Hostname     string     `json:"hostname"`
	OS           string     `json:"os"`
	Kernel       string     `json:"kernel"`
	Arch         string     `json:"arch"`
	CPUCount     int        `json:"cpu_count"`
	AgentVersion string     `json:"agent_version"`
	PanelHost    bool       `json:"panel_host"`
	Online       bool       `json:"online"`
	EnrolledAt   *time.Time `json:"enrolled_at"`
	LastSeenAt   *time.Time `json:"last_seen_at"`
	Addresses    []string   `json:"addresses"`
	PublicIPv4   *string    `json:"public_ipv4"`
	PublicIPv6   *string    `json:"public_ipv6"`
	CreatedAt    time.Time  `json:"created_at"`
}

func (s serverInfo) status() string {
	switch {
	case s.EnrolledAt == nil:
		return "not enrolled"
	case s.Online:
		return "online"
	}
	return "offline"
}

func (a *app) serversCmd() *command {
	return &command{
		name:    "servers",
		summary: "List and show servers",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List servers",
				run: func(fs *flag.FlagSet) func([]string) error {
					return func([]string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						var list []serverInfo
						raw, err := c.get(a.ctx, "servers", &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, s := range list {
							rows = append(rows, []string{s.ID, s.Name, s.Hostname, s.status(), s.OS, s.AgentVersion})
						}
						return a.table([]string{"ID", "NAME", "HOSTNAME", "STATUS", "OS", "AGENT"}, rows, "No servers.")
					}
				},
			},
			{
				name: "get", aliases: []string{"show"}, summary: "Show a server", args: []string{"SERVER"},
				long:     "Show a server. SERVER is its ID or name.",
				examples: []string{"ekctl servers get web1"},
				run: func(fs *flag.FlagSet) func([]string) error {
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "server", "servers", args[0])
						if err != nil {
							return err
						}
						var s serverInfo
						raw, err := c.get(a.ctx, "servers/"+id, &s)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						return a.details([][2]string{
							{"ID", s.ID}, {"Name", s.Name}, {"Status", s.status()}, {"Hostname", s.Hostname},
							{"OS", s.OS}, {"Kernel", s.Kernel}, {"Arch", s.Arch}, {"CPUs", strconv.Itoa(s.CPUCount)},
							{"Agent", s.AgentVersion}, {"Panel host", yesNo(s.PanelHost)},
							{"Addresses", strings.Join(s.Addresses, ", ")},
							{"Public IPv4", str(s.PublicIPv4)}, {"Public IPv6", str(s.PublicIPv6)},
							{"Last seen", fmtTimePtr(s.LastSeenAt)}, {"Created", fmtTime(s.CreatedAt)},
						})
					}
				},
			},
		},
	}
}

// orJSON finishes a command after its request: the error, or the raw answer for --json.
func (a *app) orJSON(raw []byte, err error) error {
	if err != nil {
		return err
	}
	return a.printJSON(raw)
}

// ---- Sites ----

type siteInfo struct {
	ID                string    `json:"id"`
	ServerID          string    `json:"server_id"`
	ServerName        string    `json:"server_name"`
	Domain            string    `json:"domain"`
	DomainDisplay     string    `json:"domain_display"`
	AliasesDisplay    []string  `json:"aliases_display"`
	Aliases           []string  `json:"aliases"`
	Kind              string    `json:"kind"`
	PHPVersion        *string   `json:"php_version"`
	TLS               string    `json:"tls"`
	LinuxUser         string    `json:"linux_user"`
	App               *string   `json:"app"`
	State             string    `json:"state"`
	Message           *string   `json:"message"`
	Generation        int64     `json:"generation"`
	AppliedGeneration int64     `json:"applied_generation"`
	OwnerName         *string   `json:"owner_name"`
	DiskBytes         *int64    `json:"disk_bytes"`
	CreatedAt         time.Time `json:"created_at"`
}

func (s siteInfo) name() string {
	if s.DomainDisplay != "" {
		return s.DomainDisplay
	}
	return s.Domain
}

func (a *app) sitesCmd() *command {
	return &command{
		name:    "sites",
		summary: "Manage websites",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List websites",
				run: func(fs *flag.FlagSet) func([]string) error {
					server := fs.String("server", "", "only the sites on this `server` (ID or name)")
					return func([]string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						path := "sites"
						if *server != "" {
							id, err := a.resolve(c, "server", "servers", *server)
							if err != nil {
								return err
							}
							path += "?server_id=" + url.QueryEscape(id)
						}
						var list []siteInfo
						raw, err := c.get(a.ctx, path, &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, s := range list {
							rows = append(rows, []string{s.ID, s.name(), s.Kind, str(s.PHPVersion), s.ServerName, s.State})
						}
						return a.table([]string{"ID", "DOMAIN", "KIND", "PHP", "SERVER", "STATE"}, rows, "No sites.")
					}
				},
			},
			{
				name: "get", aliases: []string{"show"}, summary: "Show a website", args: []string{"SITE"},
				long:     "Show a website. SITE is its ID or domain.",
				examples: []string{"ekctl sites get example.com"},
				run: func(fs *flag.FlagSet) func([]string) error {
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "site", "sites", args[0])
						if err != nil {
							return err
						}
						var s siteInfo
						raw, err := c.get(a.ctx, "sites/"+id, &s)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						return a.printSite(s)
					}
				},
			},
			{
				name: "create", summary: "Create a website", args: []string{"DOMAIN"},
				usage: "DOMAIN --server SERVER [--php VERSION] [flags]",
				long: "Create a website.\n\n" +
					"Without --php or --kind the site is static. --php makes it a PHP site with that version;\n" +
					"--kind php without --php uses the panel's default PHP version.",
				examples: []string{
					"ekctl sites create example.com --server web1 --php 8.4 --wait",
					"ekctl sites create docs.example.com --server web1 --alias www.docs.example.com",
				},
				run: func(fs *flag.FlagSet) func([]string) error {
					server := fs.String("server", "", "the `server` to create it on (ID or name); required")
					php := fs.String("php", "", "PHP `version`, such as 8.4; makes a PHP site")
					kind := fs.String("kind", "", "site `kind`: static or php")
					var aliases stringList
					fs.Var(&aliases, "alias", "an extra `domain` for the site; repeat for more")
					tlsMode := fs.String("tls", "", "SSL certificate `mode`: auto (Let's Encrypt) or internal (self-signed)")
					owner := fs.String("owner", "", "administrators: create it for this customer `ID`")
					wait, timeout := waitFlags(fs, "the site is set up on its server")
					return func(args []string) error {
						if *server == "" {
							return usagef("Missing --server. Choose the server with --server ID or --server NAME (see 'ekctl servers list').")
						}
						k := *kind
						switch {
						case k == "" && *php != "":
							k = "php"
						case k == "":
							k = "static"
						case k != "static" && k != "php":
							return usagef("--kind is static or php, not %q.", k)
						case k == "static" && *php != "":
							return usagef("A static site has no PHP. Leave out --php, or use --kind php.")
						}
						c, err := a.client()
						if err != nil {
							return err
						}
						serverID, err := a.resolve(c, "server", "servers", *server)
						if err != nil {
							return err
						}
						body := map[string]any{"domain": args[0], "server_id": serverID, "kind": k}
						if k == "php" {
							v := *php
							if v == "" {
								var pv struct {
									Default string `json:"default"`
								}
								if _, err := c.get(a.ctx, "php-versions", &pv); err != nil {
									return err
								}
								v = pv.Default
							}
							body["php_version"] = v
						}
						if len(aliases) > 0 {
							body["aliases"] = []string(aliases)
						}
						if *tlsMode != "" {
							body["tls"] = *tlsMode
						}
						if *owner != "" {
							body["owner_id"] = *owner
						}
						var s siteInfo
						raw, err := c.call(a.ctx, http.MethodPost, "sites", body, &s)
						if err != nil {
							return err
						}
						if !*wait {
							if a.g.JSON {
								return a.printJSON(raw)
							}
							fmt.Fprintf(a.env.Stdout, "Created site %s (%s). It is being set up on %s.\n", s.name(), s.ID, s.ServerName)
							return nil
						}
						a.progress("Created site %s (%s). Waiting for it to be set up on %s…", s.name(), s.ID, s.ServerName)
						raw, err = a.waitApplied(c, "sites/"+s.ID, "Setting up "+s.name(), *timeout)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						fmt.Fprintf(a.env.Stdout, "Site %s is ready (%s).\n", s.name(), s.ID)
						return nil
					}
				},
			},
			{
				name: "delete", aliases: []string{"rm"}, summary: "Delete a website and its files", args: []string{"SITE"},
				long: "Delete a website and its files. SITE is its ID or domain.\n\n" +
					"ekctl asks you to type the domain to confirm. Without a terminal, pass --yes.",
				examples: []string{"ekctl sites delete example.com", "ekctl sites delete 0b6c…-…-… --yes"},
				run: func(fs *flag.FlagSet) func([]string) error {
					yes := fs.Bool("yes", false, "delete without asking")
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "site", "sites", args[0])
						if err != nil {
							return err
						}
						var s siteInfo
						if _, err := c.get(a.ctx, "sites/"+id, &s); err != nil {
							return err
						}
						if err := a.confirmDelete(*yes, "the site "+s.Domain+" and all its files", s.Domain); err != nil {
							return err
						}
						raw, err := c.call(a.ctx, http.MethodDelete, "sites/"+id, nil, nil)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						fmt.Fprintf(a.env.Stdout, "Deleting site %s. The panel removes it from %s shortly.\n", s.Domain, s.ServerName)
						return nil
					}
				},
			},
		},
	}
}

func (a *app) printSite(s siteInfo) error {
	aliases := s.AliasesDisplay
	if len(aliases) == 0 {
		aliases = s.Aliases
	}
	state := s.State
	if s.Message != nil {
		state += ": " + *s.Message
	}
	return a.details([][2]string{
		{"ID", s.ID}, {"Domain", s.name()}, {"Aliases", strings.Join(aliases, ", ")},
		{"Kind", s.Kind}, {"PHP", str(s.PHPVersion)}, {"SSL", s.TLS}, {"State", state},
		{"Server", s.ServerName}, {"Linux user", s.LinuxUser}, {"App", str(s.App)},
		{"Owner", str(s.OwnerName)}, {"Disk", fmtBytes(s.DiskBytes)}, {"Created", fmtTime(s.CreatedAt)},
	})
}

// waitApplied waits until a site, database or app is set up on its server: state active
// and the newest change applied. It returns the last answer.
func (a *app) waitApplied(c *client, path, what string, timeout time.Duration) ([]byte, error) {
	var raw []byte
	err := a.waitFor(timeout, func(ctx context.Context) (bool, error) {
		var st struct {
			State             string  `json:"state"`
			Message           *string `json:"message"`
			Generation        int64   `json:"generation"`
			AppliedGeneration int64   `json:"applied_generation"`
		}
		var err error
		if raw, err = c.get(ctx, path, &st); err != nil {
			return false, err
		}
		switch {
		case st.State == "error":
			return false, fmt.Errorf("%s failed: %s", what, str(st.Message))
		case st.State == "active" && st.AppliedGeneration >= st.Generation:
			return true, nil
		}
		return false, nil
	})
	return raw, err
}

// ---- Databases ----

type databaseInfo struct {
	ID         string    `json:"id"`
	ServerName string    `json:"server_name"`
	Engine     string    `json:"engine"`
	Name       string    `json:"name"`
	Username   string    `json:"username"`
	State      string    `json:"state"`
	Message    *string   `json:"message"`
	SizeBytes  *int64    `json:"size_bytes"`
	OwnerName  *string   `json:"owner_name"`
	CreatedAt  time.Time `json:"created_at"`
}

func (a *app) databasesCmd() *command {
	return &command{
		name:    "databases",
		aliases: []string{"db"},
		summary: "Manage MariaDB and PostgreSQL databases",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List databases",
				run: func(fs *flag.FlagSet) func([]string) error {
					server := fs.String("server", "", "only the databases on this `server` (ID or name)")
					return func([]string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						path := "databases"
						if *server != "" {
							id, err := a.resolve(c, "server", "servers", *server)
							if err != nil {
								return err
							}
							path += "?server_id=" + url.QueryEscape(id)
						}
						var list []databaseInfo
						raw, err := c.get(a.ctx, path, &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, d := range list {
							rows = append(rows, []string{d.ID, d.Name, d.Engine, d.Username, d.ServerName, d.State, fmtBytes(d.SizeBytes)})
						}
						return a.table([]string{"ID", "NAME", "ENGINE", "USER", "SERVER", "STATE", "SIZE"}, rows, "No databases.")
					}
				},
			},
			{
				name: "get", aliases: []string{"show"}, summary: "Show a database", args: []string{"DATABASE"},
				long: "Show a database. DATABASE is its ID or name.",
				run: func(fs *flag.FlagSet) func([]string) error {
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "database", "databases", args[0])
						if err != nil {
							return err
						}
						var d databaseInfo
						raw, err := c.get(a.ctx, "databases/"+id, &d)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						state := d.State
						if d.Message != nil {
							state += ": " + *d.Message
						}
						return a.details([][2]string{
							{"ID", d.ID}, {"Name", d.Name}, {"Engine", d.Engine}, {"User", d.Username},
							{"State", state}, {"Server", d.ServerName}, {"Size", fmtBytes(d.SizeBytes)},
							{"Owner", str(d.OwnerName)}, {"Created", fmtTime(d.CreatedAt)},
						})
					}
				},
			},
			{
				name: "create", summary: "Create a database and its user", args: []string{"NAME"},
				usage: "NAME --server SERVER --engine mariadb|postgresql [flags]",
				long: "Create a database and a user for it. The user's password is shown once; save it.\n\n" +
					"NAME and --user use 1-32 lowercase letters, digits and _, starting with a letter.",
				examples: []string{
					"ekctl databases create shop --server web1 --engine mariadb",
					"ekctl databases create analytics --server db1 --engine postgresql --user report --wait",
				},
				run: func(fs *flag.FlagSet) func([]string) error {
					server := fs.String("server", "", "the `server` to create it on (ID or name); required")
					engine := fs.String("engine", "", "database `engine`: mariadb or postgresql; required")
					user := fs.String("user", "", "the database user's `name` (default: the database's name)")
					owner := fs.String("owner", "", "administrators: create it for this customer `ID`")
					wait, timeout := waitFlags(fs, "the database is ready")
					return func(args []string) error {
						if *server == "" {
							return usagef("Missing --server. Choose the server with --server ID or --server NAME.")
						}
						switch *engine {
						case "mariadb", "postgresql":
						case "":
							return usagef("Missing --engine. Use --engine mariadb or --engine postgresql.")
						default:
							return usagef("--engine is mariadb or postgresql, not %q.", *engine)
						}
						c, err := a.client()
						if err != nil {
							return err
						}
						serverID, err := a.resolve(c, "server", "servers", *server)
						if err != nil {
							return err
						}
						body := map[string]any{"name": args[0], "server_id": serverID, "engine": *engine}
						if *user != "" {
							body["username"] = *user
						}
						if *owner != "" {
							body["owner_id"] = *owner
						}
						var created struct {
							Database databaseInfo `json:"database"`
							Password string       `json:"password"`
						}
						raw, err := c.call(a.ctx, http.MethodPost, "databases", body, &created)
						if err != nil {
							return err
						}
						d := created.Database
						if *wait {
							a.progress("Created database %s. Waiting for it to be ready on %s…", d.Name, d.ServerName)
							if _, err := a.waitApplied(c, "databases/"+d.ID, "Creating database "+d.Name, *timeout); err != nil {
								if a.g.JSON {
									a.printJSON(raw) // the password is shown only in this answer
								} else {
									a.printPassword(d, created.Password)
								}
								return err
							}
						}
						if a.g.JSON {
							return a.printJSON(raw)
						}
						return a.printPassword(d, created.Password)
					}
				},
			},
			{
				name: "delete", aliases: []string{"rm"}, summary: "Delete a database and its user", args: []string{"DATABASE"},
				long: "Delete a database, its data and its user. DATABASE is its ID or name.\n\n" +
					"ekctl asks you to type the name to confirm. Without a terminal, pass --yes.",
				run: func(fs *flag.FlagSet) func([]string) error {
					yes := fs.Bool("yes", false, "delete without asking")
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "database", "databases", args[0])
						if err != nil {
							return err
						}
						var d databaseInfo
						if _, err := c.get(a.ctx, "databases/"+id, &d); err != nil {
							return err
						}
						if err := a.confirmDelete(*yes, "the "+d.Engine+" database "+d.Name+" and all its data", d.Name); err != nil {
							return err
						}
						raw, err := c.call(a.ctx, http.MethodDelete, "databases/"+id, nil, nil)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						fmt.Fprintf(a.env.Stdout, "Deleting database %s. The panel removes it from %s shortly.\n", d.Name, d.ServerName)
						return nil
					}
				},
			},
		},
	}
}

func (a *app) printPassword(d databaseInfo, password string) error {
	fmt.Fprintf(a.env.Stdout, "Created %s database %s on %s.\n", d.Engine, d.Name, d.ServerName)
	if err := a.details([][2]string{{"  ID", d.ID}, {"  User", d.Username}, {"  Password", password}}); err != nil {
		return err
	}
	fmt.Fprintln(a.env.Stderr, "The password is shown only this once. Save it now.")
	return nil
}

// ---- Apps ----

type appInfo struct {
	ID            string    `json:"id"`
	ServerName    string    `json:"server_name"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"`
	Image         *string   `json:"image"`
	Domain        *string   `json:"domain"`
	DomainDisplay string    `json:"domain_display"`
	TLS           string    `json:"tls"`
	ContainerPort *int      `json:"container_port"`
	HostPort      *int      `json:"host_port"`
	Stopped       bool      `json:"stopped"`
	State         string    `json:"state"`
	Message       *string   `json:"message"`
	GitURL        *string   `json:"git_url"`
	GitBranch     *string   `json:"git_branch"`
	AutoDeploy    bool      `json:"auto_deploy"`
	OwnerName     *string   `json:"owner_name"`
	DiskBytes     *int64    `json:"disk_bytes"`
	CreatedAt     time.Time `json:"created_at"`
}

func (p appInfo) domain() string {
	if p.DomainDisplay != "" {
		return p.DomainDisplay
	}
	return str(p.Domain)
}

func (p appInfo) state() string {
	if p.Stopped && p.State == "active" {
		return "stopped"
	}
	return p.State
}

func intStr(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

type deploymentInfo struct {
	ID            string  `json:"id"`
	AppID         string  `json:"app_id"`
	Number        int     `json:"number"`
	Status        string  `json:"status"`
	Branch        string  `json:"branch"`
	CommitSHA     *string `json:"commit_sha"`
	CommitMessage *string `json:"commit_message"`
	Error         *string `json:"error"`
}

func (a *app) appsCmd() *command {
	return &command{
		name:    "apps",
		summary: "Manage Docker and Git apps",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List apps",
				run: func(fs *flag.FlagSet) func([]string) error {
					server := fs.String("server", "", "only the apps on this `server` (ID or name)")
					return func([]string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						path := "apps"
						if *server != "" {
							id, err := a.resolve(c, "server", "servers", *server)
							if err != nil {
								return err
							}
							path += "?server_id=" + url.QueryEscape(id)
						}
						var list []appInfo
						raw, err := c.get(a.ctx, path, &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, p := range list {
							rows = append(rows, []string{p.ID, p.Name, p.Kind, p.domain(), p.ServerName, p.state()})
						}
						return a.table([]string{"ID", "NAME", "KIND", "DOMAIN", "SERVER", "STATE"}, rows, "No apps.")
					}
				},
			},
			{
				name: "get", aliases: []string{"show"}, summary: "Show an app", args: []string{"APP"},
				long: "Show an app. APP is its ID or name.",
				run: func(fs *flag.FlagSet) func([]string) error {
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "app", "apps", args[0])
						if err != nil {
							return err
						}
						var p appInfo
						raw, err := c.get(a.ctx, "apps/"+id, &p)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						state := p.state()
						if p.Message != nil {
							state += ": " + *p.Message
						}
						rows := [][2]string{
							{"ID", p.ID}, {"Name", p.Name}, {"Kind", p.Kind}, {"State", state},
							{"Domain", p.domain()}, {"SSL", p.TLS}, {"Image", str(p.Image)},
							{"Container port", intStr(p.ContainerPort)}, {"Host port", intStr(p.HostPort)},
						}
						if p.Kind == "git" {
							rows = append(rows, [2]string{"Git", str(p.GitURL)}, [2]string{"Branch", str(p.GitBranch)},
								[2]string{"Auto deploy", yesNo(p.AutoDeploy)})
						}
						rows = append(rows, [2]string{"Server", p.ServerName}, [2]string{"Owner", str(p.OwnerName)},
							[2]string{"Disk", fmtBytes(p.DiskBytes)}, [2]string{"Created", fmtTime(p.CreatedAt)})
						return a.details(rows)
					}
				},
			},
			{
				name: "restart", summary: "Restart an app's containers", args: []string{"APP"},
				long: "Restart an app's containers. APP is its ID or name.",
				run: func(fs *flag.FlagSet) func([]string) error {
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "app", "apps", args[0])
						if err != nil {
							return err
						}
						raw, err := c.call(a.ctx, http.MethodPost, "apps/"+id+"/restart", nil, nil)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						fmt.Fprintf(a.env.Stdout, "Restarted app %s.\n", args[0])
						return nil
					}
				},
			},
			{
				name: "deploy", summary: "Deploy a Git app now", args: []string{"APP"},
				long: "Build and deploy a Git app from its branch now. APP is its ID or name.\n\n" +
					"With --wait, ekctl waits until the new version is live (or the build fails).",
				examples: []string{"ekctl apps deploy shop-api --wait"},
				run: func(fs *flag.FlagSet) func([]string) error {
					wait, timeout := waitFlags(fs, "the deployment is live or has failed")
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "app", "apps", args[0])
						if err != nil {
							return err
						}
						var d deploymentInfo
						raw, err := c.call(a.ctx, http.MethodPost, "apps/"+id+"/deploy", nil, &d)
						if err != nil {
							return err
						}
						if !*wait {
							if a.g.JSON {
								return a.printJSON(raw)
							}
							fmt.Fprintf(a.env.Stdout, "Started deployment #%d of %s (%s).\n", d.Number, args[0], d.ID)
							return nil
						}
						a.progress("Started deployment #%d of %s. Waiting for it…", d.Number, args[0])
						path := "apps/" + id + "/deployments/" + d.ID
						last := d.Status
						err = a.waitFor(*timeout, func(ctx context.Context) (bool, error) {
							var err error
							if raw, err = c.get(ctx, path, &d); err != nil {
								return false, err
							}
							if d.Status != last {
								last = d.Status
								a.progress("Deployment #%d: %s", d.Number, d.Status)
							}
							switch d.Status {
							case "live", "superseded":
								return true, nil
							case "failed", "cancelled":
								return false, fmt.Errorf("Deployment #%d %s: %s\nSee its log with: ekctl api GET %s/log", d.Number, d.Status, str(d.Error), path)
							}
							return false, nil
						})
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						fmt.Fprintf(a.env.Stdout, "Deployment #%d of %s is live.\n", d.Number, args[0])
						return nil
					}
				},
			},
		},
	}
}
