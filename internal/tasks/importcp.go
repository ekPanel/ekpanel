package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/cron"
	"ekpanel/internal/store"
)

// KindImportCPanel fills websites and databases made from a cPanel backup (the panel made
// them already): it applies the server, has the agent put the files and dumps in place,
// then adds the cron jobs to the main website.
const KindImportCPanel = "import.cpanel"

type ImportSite struct {
	SiteID  string `json:"site_id"`
	DocRoot string `json:"docroot"`
	Main    bool   `json:"main"`
}

type ImportCPanelParams struct {
	Server    string       `json:"server"`
	File      string       `json:"file"`
	CPUser    string       `json:"cp_user"`
	Sites     []ImportSite `json:"sites"`
	Databases []string     `json:"databases"` // database IDs
	Cron      []string     `json:"cron"`
	// Mail are mailboxes (made already) whose messages are copied in.
	Mail []agentproto.ImportMailbox `json:"mail,omitempty"`
	// Report holds what the panel already knows: what is not imported and why.
	Report ImportReport `json:"report"`
}

// ImportReport is what the import did, shown when it ends.
type ImportReport struct {
	Sites     []string `json:"sites"`
	Databases []string `json:"databases"`
	Cron      int      `json:"cron"`
	// Email: domains made, mailboxes and forwarders in them, messages copied in.
	MailDomains  []string `json:"mail_domains,omitempty"`
	Mailboxes    int      `json:"mailboxes,omitempty"`
	Forwarders   int      `json:"forwarders,omitempty"`
	MailMessages int      `json:"mail_messages,omitempty"`
	MailFailed   int      `json:"mail_failed,omitempty"`
	// NewMailPassword are mailboxes (addresses) whose password could not be kept.
	NewMailPassword []string `json:"new_mail_password,omitempty"`
	// Zones are the DNS zones made on ekPanel's name servers.
	Zones       []string `json:"zones,omitempty"`
	Files       int64    `json:"files"`
	Bytes       int64    `json:"bytes"`
	NewPassword []string `json:"new_password,omitempty"` // databases whose password could not be kept
	// PathsChanged are files (relative to /home) where /home/<cPanel user>/ was changed.
	PathsChanged      []string     `json:"paths_changed,omitempty"`
	PathsChangedCount int          `json:"paths_changed_count,omitempty"`
	NotImported       []ImportNote `json:"not_imported"`
}

// ImportNote is one thing not imported, for the UI to put in words: Code says what, the
// other fields fill in the sentence.
//
//	certificate (Name: domain)          mail (Count: accounts, Domains)
//	dns_zone (Name)                     folder (Name: top folder of the home, Count: entries)
//	links_outside (Count)               special_files (Count)
//	db_user (Name: user, Detail: database, which has a user of its own name now)
//	cron_line (Name: the line)          cron_setting (Name: MAILTO=… and such)
//	cron_no_main (Count: jobs, as the main website was not imported)
//	mail (Name: domain not chosen, Count: mailboxes)   mail_mailbox, mail_forwarder, mail_mdbox (Name: address)
//	mail_unusable (Name: forwarding line, Detail: domain)
//	mail_catch_all (Name: the address unknown mail went to, not a mailbox here; Detail: domain)
//	zone_mx_here, zone_old_ip, zone_record, zone_unread, zone_failed (see FilterCPanelZone)
type ImportNote struct {
	Code    string `json:"code"`
	Name    string `json:"name,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Count   int    `json:"count,omitempty"`
	Domains int    `json:"domains,omitempty"`
}

func (r *Runner) QueueImportCPanel(ctx context.Context, p ImportCPanelParams, by *string) (store.Task, error) {
	return r.Enqueue(ctx, store.NewTask{Kind: KindImportCPanel, ServerID: &p.Server, Params: p, CreatedBy: by, MaxAttempts: 1})
}

func (r *Runner) importCPanel(ctx context.Context, t store.Task) (any, error) {
	var p ImportCPanelParams
	if err := json.Unmarshal(t.Params, &p); err != nil {
		return nil, err
	}
	rep := p.Report
	if _, err := r.Reconcile(ctx, p.Server); err != nil {
		return rep, fmt.Errorf("set up the websites and databases: %w", err)
	}
	apply := agentproto.ImportApplyParams{File: p.File, Mail: p.Mail}
	if agentproto.ValidCPanelUser(p.CPUser) {
		apply.OldUser = p.CPUser
	}
	var main *store.Site
	for _, is := range p.Sites {
		s, err := r.store.GetSite(ctx, is.SiteID)
		if err != nil {
			return rep, err
		}
		if s.State != "active" {
			return rep, fmt.Errorf("the website %s could not be set up", s.Domain)
		}
		apply.Sites = append(apply.Sites, agentproto.ImportSite{DocRoot: is.DocRoot, User: s.LinuxUser})
		if is.Main {
			main = &s
			apply.MainUser = s.LinuxUser
		}
	}
	for _, id := range p.Databases {
		d, err := r.store.GetDatabase(ctx, id)
		if err != nil {
			return rep, err
		}
		if d.State != "active" {
			return rep, fmt.Errorf("the database %s could not be set up", d.Name)
		}
		apply.Databases = append(apply.Databases, agentproto.TransferDatabase{Engine: d.Engine, Name: d.Name, User: d.Username})
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Hour)
	defer cancel()
	raw, err := r.hub.Call(callCtx, p.Server, agentproto.OpImportApply, apply)
	if err != nil {
		return rep, fmt.Errorf("copy the files and databases: %w", err)
	}
	var res agentproto.ImportApplyResult
	json.Unmarshal(raw, &res)
	rep.Files, rep.Bytes, rep.Databases = res.Files, res.Bytes, res.Databases
	rep.PathsChanged, rep.PathsChangedCount = res.PathsChanged, res.PathsChangedCount
	rep.MailMessages, rep.MailFailed = res.MailMessages, res.MailFailed
	for _, sk := range res.Skipped {
		switch sk.Name {
		case "links_outside", "special_files":
			rep.NotImported = append(rep.NotImported, ImportNote{Code: sk.Name, Count: sk.Count})
		default:
			rep.NotImported = append(rep.NotImported, ImportNote{Code: "folder", Name: sk.Name, Count: sk.Count})
		}
	}

	// Cron jobs go to the main website, with the old home folder's path changed.
	if main == nil && len(p.Cron) > 0 {
		rep.NotImported = append(rep.NotImported, ImportNote{Code: "cron_no_main", Count: len(p.Cron)})
	}
	if main != nil {
		for _, line := range p.Cron {
			sched, cmd, ok := splitCronLine(line)
			if !ok {
				rep.NotImported = append(rep.NotImported, ImportNote{Code: "cron_line", Name: line})
				continue
			}
			if p.CPUser != "" {
				cmd = strings.ReplaceAll(cmd, "/home/"+p.CPUser+"/", "/home/"+main.LinuxUser+"/")
			}
			if main.PHPVersion != nil {
				cmd = cpanelPHPRE.ReplaceAllString(cmd, "${1}php"+*main.PHPVersion+"${2}")
			}
			name := cmd
			if len([]rune(name)) > 60 {
				name = string([]rune(name)[:60]) + "…"
			}
			if _, err := r.store.CreateCronJob(ctx, store.NewCronJob{ServerID: p.Server, SiteID: &main.ID, CreatedBy: t.CreatedBy,
				CronSettings: store.CronSettings{Name: name, Schedule: sched, Timezone: "UTC", Command: cmd, TimeoutSeconds: 3600, Enabled: true}}); err != nil {
				rep.NotImported = append(rep.NotImported, ImportNote{Code: "cron_line", Name: line})
				continue
			}
			rep.Cron++
		}
		if rep.Cron > 0 {
			if _, err := r.Reconcile(ctx, p.Server); err != nil {
				r.log.Warn("apply imported cron jobs", "err", err)
			}
		}
	}
	return rep, nil
}

// cpanelPHPRE matches the PHP binaries cPanel's cron jobs call (/usr/local/bin/php,
// /opt/cpanel/ea-php82/root/usr/bin/php, ea-php82 …): here the website's own version runs.
var cpanelPHPRE = regexp.MustCompile(`(^|[\s;&|(])(?:/opt/cpanel/ea-php\d{2}/root)?(?:/usr(?:/local)?/bin/)?(?:ea-php\d{2}|php(?:\d{2})?)(?:-cli)?(\s|$)`)

// splitCronLine splits "m h dom mon dow command" (or "@daily command").
func splitCronLine(line string) (schedule, command string, ok bool) {
	f := strings.Fields(line)
	n := 5
	if len(f) > 0 && strings.HasPrefix(f[0], "@") {
		n = 1
	}
	if len(f) <= n {
		return "", "", false
	}
	schedule = strings.Join(f[:n], " ")
	if _, err := cron.Parse(schedule); err != nil {
		return "", "", false
	}
	// The command keeps its own spacing after the schedule.
	rest := strings.TrimSpace(line)
	for range n {
		rest = strings.TrimSpace(rest)
		if i := strings.IndexAny(rest, " \t"); i >= 0 {
			rest = rest[i:]
		}
	}
	return schedule, strings.TrimSpace(rest), strings.TrimSpace(rest) != ""
}
