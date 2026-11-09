package cli

import (
	"flag"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

type zoneInfo struct {
	ID          string   `json:"id"`
	AccountName string   `json:"account_name"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	NameServers []string `json:"name_servers"`
	Proxied     bool     `json:"proxied"`
	SyncError   *string  `json:"sync_error"`
	Records     int      `json:"records"`
}

type cfRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	TTL      int    `json:"ttl"`
	Proxied  bool   `json:"proxied"`
	Priority *int   `json:"priority"`
	Managed  bool   `json:"managed"`
}

type zoneDetail struct {
	Zone    zoneInfo   `json:"zone"`
	Records []cfRecord `json:"records"`
}

func ttlStr(ttl int) string {
	if ttl <= 1 {
		return "auto"
	}
	return strconv.Itoa(ttl)
}

func (a *app) dnsCmd() *command {
	return &command{
		name:    "dns",
		summary: "Manage DNS zones and records",
		long: "Manage DNS zones and records.\n\n" +
			"'zones' and 'records' are zones kept in Cloudflare. 'hosted' are zones on ekPanel's own name servers.",
		subs: []*command{
			{
				name: "zones", summary: "Cloudflare zones",
				subs: []*command{{
					name: "list", aliases: []string{"ls"}, summary: "List Cloudflare zones",
					run: func(fs *flag.FlagSet) func([]string) error {
						return func([]string) error {
							c, err := a.client()
							if err != nil {
								return err
							}
							var list []zoneInfo
							raw, err := c.get(a.ctx, "dns/zones", &list)
							if err != nil || a.g.JSON {
								return a.orJSON(raw, err)
							}
							rows := [][]string{}
							for _, z := range list {
								status := z.Status
								if z.SyncError != nil {
									status += " (sync error)"
								}
								rows = append(rows, []string{z.ID, z.Name, status, z.AccountName, strconv.Itoa(z.Records)})
							}
							return a.table([]string{"ID", "NAME", "STATUS", "ACCOUNT", "EKPANEL RECORDS"}, rows, "No Cloudflare zones.")
						}
					},
				}},
			},
			{
				name: "records", summary: "Records of Cloudflare zones",
				subs: []*command{
					{
						name: "list", aliases: []string{"ls"}, summary: "List a Cloudflare zone's records", args: []string{"ZONE"},
						long: "List a Cloudflare zone's records. ZONE is the zone's ID or name.\n\n" +
							"MANAGED records belong to a website or app; they change and go with it.",
						examples: []string{"ekctl dns records list example.com"},
						run: func(fs *flag.FlagSet) func([]string) error {
							return func(args []string) error {
								c, err := a.client()
								if err != nil {
									return err
								}
								id, err := a.resolve(c, "zone", "dns/zones", args[0])
								if err != nil {
									return err
								}
								var z zoneDetail
								raw, err := c.get(a.ctx, "dns/zones/"+id, &z)
								if err != nil || a.g.JSON {
									return a.orJSON(raw, err)
								}
								rows := [][]string{}
								for _, r := range z.Records {
									content := r.Content
									if r.Priority != nil {
										content = strconv.Itoa(*r.Priority) + " " + content
									}
									rows = append(rows, []string{r.ID, r.Type, r.Name, truncate(content, 60), ttlStr(r.TTL), yesNo(r.Proxied), yesNo(r.Managed)})
								}
								return a.table([]string{"ID", "TYPE", "NAME", "CONTENT", "TTL", "PROXIED", "MANAGED"}, rows, "No records.")
							}
						},
					},
					{
						name: "add", aliases: []string{"create"}, summary: "Add a record to a Cloudflare zone", args: []string{"ZONE"},
						usage: "ZONE --type TYPE --name NAME --content CONTENT [flags]",
						long: "Add a record to a Cloudflare zone. ZONE is the zone's ID or name.\n\n" +
							"NAME is www, @ for the zone itself, or a full host name in the zone.",
						examples: []string{
							"ekctl dns records add example.com --type A --name www --content 203.0.113.7 --proxied",
							"ekctl dns records add example.com --type MX --name @ --content mail.example.com --priority 10",
							"ekctl dns records add example.com --type TXT --name @ --content 'v=spf1 mx -all'",
						},
						run: func(fs *flag.FlagSet) func([]string) error {
							typ := fs.String("type", "", "record `type`: A, AAAA, CNAME, MX, TXT, NS, CAA …; required")
							name := fs.String("name", "", "record `name`; required")
							content := fs.String("content", "", "what the record points at, or its text; required")
							ttl := fs.Int("ttl", 1, "time to live in `seconds` (60-86400); 1 is automatic")
							proxied := fs.Bool("proxied", false, "send traffic through Cloudflare (A, AAAA and CNAME)")
							priority := fs.Int("priority", -1, "MX `priority` (0-65535)")
							comment := fs.String("comment", "", "a `note` kept with the record in Cloudflare")
							return func(args []string) error {
								for _, f := range []struct{ flag, val string }{{"--type", *typ}, {"--name", *name}, {"--content", *content}} {
									if f.val == "" {
										return usagef("Missing %s.", f.flag)
									}
								}
								rec := map[string]any{"type": strings.ToUpper(*typ), "name": *name, "content": *content, "ttl": *ttl, "proxied": *proxied}
								if *priority >= 0 {
									rec["priority"] = *priority
								} else if strings.EqualFold(*typ, "MX") {
									return usagef("An MX record needs --priority, such as --priority 10.")
								}
								if *comment != "" {
									rec["comment"] = *comment
								}
								c, err := a.client()
								if err != nil {
									return err
								}
								id, err := a.resolve(c, "zone", "dns/zones", args[0])
								if err != nil {
									return err
								}
								var r cfRecord
								raw, err := c.call(a.ctx, http.MethodPost, "dns/zones/"+id+"/records", rec, &r)
								if err != nil || a.g.JSON {
									return a.orJSON(raw, err)
								}
								fmt.Fprintf(a.env.Stdout, "Added %s %s → %s (%s).\n", r.Type, r.Name, r.Content, r.ID)
								return nil
							}
						},
					},
					{
						name: "delete", aliases: []string{"rm"}, summary: "Delete a record from a Cloudflare zone", args: []string{"ZONE", "RECORD_ID"},
						long: "Delete a record from a Cloudflare zone. ZONE is the zone's ID or name; RECORD_ID is from\n" +
							"'ekctl dns records list'. ekctl asks you to type the record's name to confirm. Without a\n" +
							"terminal, pass --yes. Records that belong to a website or app cannot be deleted here.",
						run: func(fs *flag.FlagSet) func([]string) error {
							yes := fs.Bool("yes", false, "delete without asking")
							return func(args []string) error {
								c, err := a.client()
								if err != nil {
									return err
								}
								id, err := a.resolve(c, "zone", "dns/zones", args[0])
								if err != nil {
									return err
								}
								name := args[1]
								if !*yes {
									var z zoneDetail
									if _, err := c.get(a.ctx, "dns/zones/"+id, &z); err != nil {
										return err
									}
									var found *cfRecord
									for i := range z.Records {
										if z.Records[i].ID == args[1] {
											found = &z.Records[i]
										}
									}
									if found == nil {
										return fmt.Errorf("The zone %s has no record with the ID %s. See 'ekctl dns records list %s'.", z.Zone.Name, args[1], args[0])
									}
									if err := a.confirmDelete(false, "the "+found.Type+" record "+found.Name+" → "+found.Content, found.Name); err != nil {
										return err
									}
									name = found.Type + " " + found.Name
								}
								raw, err := c.call(a.ctx, http.MethodDelete, "dns/zones/"+id+"/records/"+args[1], nil, nil)
								if err != nil || a.g.JSON {
									return a.orJSON(raw, err)
								}
								fmt.Fprintf(a.env.Stdout, "Deleted the record %s.\n", name)
								return nil
							}
						},
					},
				},
			},
			{
				name: "hosted", summary: "Zones on ekPanel's own name servers",
				subs: []*command{
					{
						name: "list", aliases: []string{"ls"}, summary: "List hosted zones",
						run: func(fs *flag.FlagSet) func([]string) error {
							return func([]string) error {
								c, err := a.client()
								if err != nil {
									return err
								}
								var list []struct {
									ID        string  `json:"id"`
									Name      string  `json:"name"`
									DNSSEC    bool    `json:"dnssec"`
									Records   int     `json:"records"`
									Error     *string `json:"error"`
									OwnerName *string `json:"owner_name"`
								}
								raw, err := c.get(a.ctx, "dns/hosted", &list)
								if err != nil || a.g.JSON {
									return a.orJSON(raw, err)
								}
								rows := [][]string{}
								for _, z := range list {
									rows = append(rows, []string{z.ID, z.Name, yesNo(z.DNSSEC), strconv.Itoa(z.Records), str(z.OwnerName), truncate(str(z.Error), 50)})
								}
								return a.table([]string{"ID", "NAME", "DNSSEC", "RECORDS", "OWNER", "ERROR"}, rows, "No hosted zones.")
							}
						},
					},
					{
						name: "records", summary: "List a hosted zone's records", args: []string{"ZONE"},
						long: "List a hosted zone's records. ZONE is the zone's ID or name.\n\n" +
							"Records with SOURCE 'ekpanel' are made for websites, apps and mail; they have no ID.",
						run: func(fs *flag.FlagSet) func([]string) error {
							return func(args []string) error {
								c, err := a.client()
								if err != nil {
									return err
								}
								id, err := a.resolve(c, "hosted zone", "dns/hosted", args[0])
								if err != nil {
									return err
								}
								type rec struct {
									ID       string `json:"id"`
									Name     string `json:"name"`
									Type     string `json:"type"`
									Content  string `json:"content"`
									Priority int    `json:"priority"`
									TTL      int    `json:"ttl"`
								}
								var z struct {
									Kept []rec `json:"kept"`
									Made []rec `json:"made"`
								}
								raw, err := c.get(a.ctx, "dns/hosted/"+id, &z)
								if err != nil || a.g.JSON {
									return a.orJSON(raw, err)
								}
								rows := [][]string{}
								add := func(list []rec, source string) {
									for _, r := range list {
										content := r.Content
										if r.Priority != 0 || r.Type == "MX" {
											content = strconv.Itoa(r.Priority) + " " + content
										}
										rows = append(rows, []string{r.ID, r.Type, r.Name, truncate(content, 60), strconv.Itoa(r.TTL), source})
									}
								}
								add(z.Kept, "you")
								add(z.Made, "ekpanel")
								return a.table([]string{"ID", "TYPE", "NAME", "CONTENT", "TTL", "SOURCE"}, rows, "No records.")
							}
						},
					},
				},
			},
		},
	}
}
