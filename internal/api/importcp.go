package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
	"ekpanel/internal/tasks"
)

// Importing a cPanel account (administrators): inspect a backup archive on a server, then
// make the chosen websites and databases and fill them (tasks.KindImportCPanel).

type importInspectRequest struct {
	ServerID string `json:"server_id"`
	File     string `json:"file"`
}

// importInspection is the plan with what stands in the way on this panel.
type importInspection struct {
	agentproto.CPanelPlan
	// TakenDomains are host names other websites or apps already use.
	TakenDomains []string `json:"taken_domains"`
	// TakenDatabases are names (or users) the server already has.
	TakenDatabases []string `json:"taken_databases"`
	PHPVersions    []string `json:"php_versions"`
	// NewPasswordDatabases and NewPasswordMailboxes (addresses) cannot keep their
	// passwords; the hashes themselves are not sent.
	NewPasswordDatabases []string `json:"new_password_databases"`
	NewPasswordMailboxes []string `json:"new_password_mailboxes"`
	// MailEnabled: the server has email, so mail domains can be imported.
	MailEnabled bool     `json:"mail_enabled"`
	TakenMail   []string `json:"taken_mail"`
	// NameServers: ekPanel's name servers are set up, so zones can be imported.
	NameServers bool     `json:"name_servers"`
	TakenZones  []string `json:"taken_zones"`
}

type cachedImportPlan struct {
	plan agentproto.CPanelPlan
	at   time.Time
}

// importPlanTTL is how long an inspection stands for the import that follows it.
const importPlanTTL = time.Hour

// inspectImport reads the archive on the server; fresh says whether a recent reading may be used.
func (s *Server) inspectImport(ctx context.Context, serverID, file string, fresh bool) (agentproto.CPanelPlan, error) {
	key := serverID + "/" + file
	if !fresh {
		if v, ok := s.importPlans.Load(key); ok && time.Since(v.(cachedImportPlan).at) < importPlanTTL {
			return v.(cachedImportPlan).plan, nil
		}
	}
	var plan agentproto.CPanelPlan
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	raw, err := s.hub.Call(callCtx, serverID, agentproto.OpImportInspect, agentproto.ImportInspectParams{File: file})
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return plan, err
	}
	s.importPlans.Store(key, cachedImportPlan{plan: plan, at: time.Now()})
	return plan, nil
}

func (s *Server) importServer(w http.ResponseWriter, r *http.Request, serverID, file string) (store.Server, bool) {
	if !isUUID(serverID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return store.Server{}, false
	}
	srv, err := s.store.GetServer(r.Context(), serverID)
	if s.serverLookupFailed(w, r, err) {
		return srv, false
	}
	if err := agentproto.ValidImportFile(file); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_import_file", "name a .tar.gz backup in "+agentproto.ImportDir+" on the server")
		return srv, false
	}
	return srv, true
}

func (s *Server) handleInspectImport(w http.ResponseWriter, r *http.Request) {
	var req importInspectRequest
	if !readJSON(w, r, &req) {
		return
	}
	srv, ok := s.importServer(w, r, req.ServerID, req.File)
	if !ok {
		return
	}
	plan, err := s.inspectImport(r.Context(), srv.ID, req.File, true)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	out := importInspection{CPanelPlan: plan, TakenDomains: []string{}, TakenDatabases: []string{}, PHPVersions: agentproto.PHPVersions,
		NewPasswordDatabases: []string{}, NewPasswordMailboxes: []string{}, TakenMail: []string{}, TakenZones: []string{}}
	// The plan is shared with the cache: copy what is changed for the answer.
	out.Databases = slices.Clone(plan.Databases)
	for i := range out.Databases {
		if out.Databases[i].PasswordHash == "" {
			out.NewPasswordDatabases = append(out.NewPasswordDatabases, out.Databases[i].Name)
		}
		out.Databases[i].PasswordHash = ""
	}
	out.Mail = slices.Clone(plan.Mail)
	var mailNames []string
	for i, d := range out.Mail {
		mailNames = append(mailNames, d.Domain)
		out.Mail[i].Mailboxes = slices.Clone(d.Mailboxes)
		for j, mb := range out.Mail[i].Mailboxes {
			if !agentproto.ValidSHA512Crypt(mb.PasswordHash) {
				out.NewPasswordMailboxes = append(out.NewPasswordMailboxes, mb.Name+"@"+d.Domain)
			}
			out.Mail[i].Mailboxes[j].PasswordHash = ""
		}
	}
	if _, err := s.store.GetMailServer(r.Context(), srv.ID); err == nil {
		out.MailEnabled = true
	}
	if taken, err := s.store.MailDomainsTaken(r.Context(), mailNames); err != nil {
		s.internalError(w, r, err)
		return
	} else if taken != nil {
		out.TakenMail = taken
	}
	ns, err := s.store.ListNameServers(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.NameServers = slices.ContainsFunc(ns, func(n store.NameServer) bool { return n.Primary })
	for _, z := range plan.Zones {
		if taken, err := s.store.ZoneNameTaken(r.Context(), z.Name); err != nil {
			s.internalError(w, r, err)
			return
		} else if taken {
			out.TakenZones = append(out.TakenZones, z.Name)
		}
	}
	var names []string
	for _, d := range plan.Domains {
		names = append(names, d.Domain)
		names = append(names, d.Aliases...)
	}
	if out.TakenDomains, err = s.store.HostnamesTaken(r.Context(), names); err != nil {
		s.internalError(w, r, err)
		return
	}
	dbs, err := s.store.ListDatabases(r.Context(), srv.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, d := range plan.Databases {
		if slices.ContainsFunc(dbs, func(x store.Database) bool {
			return x.Engine == "mariadb" && (x.Name == d.Name || x.Username == d.User)
		}) {
			out.TakenDatabases = append(out.TakenDatabases, d.Name)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type importRequest struct {
	ServerID  string            `json:"server_id"`
	File      string            `json:"file"`
	OwnerID   string            `json:"owner_id"`
	Sites     []string          `json:"sites"`     // domains to import
	PHP       map[string]string `json:"php"`       // domain → PHP version (optional)
	Databases []string          `json:"databases"` // database names to import
	Cron      bool              `json:"cron"`      // the cron jobs, onto the main website
	Mail      []string          `json:"mail"`      // mail domains to import
	Zones     []string          `json:"zones"`     // DNS zones to make on ekPanel's name servers
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req importRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	srv, ok := s.importServer(w, r, req.ServerID, req.File)
	if !ok {
		return
	}
	if len(req.Sites)+len(req.Databases)+len(req.Mail)+len(req.Zones) == 0 {
		writeError(w, http.StatusBadRequest, "nothing_to_import", "choose at least one website, database, mail domain or zone")
		return
	}
	// The archive decides what there is, not the request (read again unless inspected lately).
	plan, err := s.inspectImport(ctx, srv.ID, req.File, false)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	place, ok := s.resolveOwner(w, r, req.OwnerID)
	if !ok {
		return
	}
	var domains []agentproto.CPanelDomain
	for _, name := range req.Sites {
		i := slices.IndexFunc(plan.Domains, func(d agentproto.CPanelDomain) bool { return d.Domain == name })
		if i < 0 {
			writeErrorData(w, http.StatusBadRequest, "not_in_backup", "the backup has no website "+name, map[string]any{"name": name})
			return
		}
		domains = append(domains, plan.Domains[i])
	}
	var dbs []agentproto.CPanelDatabase
	for _, name := range req.Databases {
		i := slices.IndexFunc(plan.Databases, func(d agentproto.CPanelDatabase) bool { return d.Name == name })
		if i < 0 {
			writeErrorData(w, http.StatusBadRequest, "not_in_backup", "the backup has no database "+name, map[string]any{"name": name})
			return
		}
		dbs = append(dbs, plan.Databases[i])
	}
	// Package limits and server, for everything at once.
	if place.Package != nil {
		pkg := place.Package
		if len(pkg.ServerIDs) > 0 && !slices.Contains(pkg.ServerIDs, srv.ID) {
			writeError(w, http.StatusBadRequest, "server_not_allowed", "this package cannot use that server")
			return
		}
		usage, err := s.store.OwnerUsage(ctx, *place.OwnerID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		cronN := 0
		if req.Cron {
			cronN = len(plan.Cron)
		}
		switch {
		case usage.Sites+len(domains) > pkg.MaxSites:
			writeError(w, http.StatusConflict, "limit_reached_site", "the package does not allow that many websites")
			return
		case usage.Databases+len(dbs) > pkg.MaxDatabases:
			writeError(w, http.StatusConflict, "limit_reached_database", "the package does not allow that many databases")
			return
		case usage.CronJobs+cronN > pkg.MaxCronJobs:
			writeError(w, http.StatusConflict, "limit_reached_cron", "the package does not allow that many cron jobs")
			return
		}
	}
	// Names that are taken stop the import before anything is made.
	var names []string
	for _, d := range domains {
		names = append(names, d.Domain)
		names = append(names, d.Aliases...)
	}
	if taken, err := s.store.HostnamesTaken(ctx, names); err != nil {
		s.internalError(w, r, err)
		return
	} else if len(taken) > 0 {
		writeErrorData(w, http.StatusConflict, "import_domains_taken", "already used here: "+strings.Join(taken, ", "), map[string]any{"names": strings.Join(taken, ", ")})
		return
	}
	owned, err := s.ownedHosts(ctx, place, domains)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	mailDomains, ok := s.checkImportMail(w, r, srv, place, plan, req.Mail, owned)
	if !ok {
		return
	}
	zones, ok := s.checkImportZones(w, r, place, plan, req.Zones, owned)
	if !ok {
		return
	}
	existing, err := s.store.ListDatabases(ctx, srv.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	report := tasks.ImportReport{Sites: []string{}, Databases: []string{}, NotImported: []tasks.ImportNote{}}
	type dbPlan struct {
		name, user, hash string
	}
	var dbPlans []dbPlan
	usedUsers := map[string]bool{}
	for _, x := range existing {
		usedUsers[x.Username] = true
	}
	for _, d := range dbs {
		if slices.ContainsFunc(existing, func(x store.Database) bool { return x.Engine == "mariadb" && x.Name == d.Name }) || !dbNameRE.MatchString(d.Name) {
			writeErrorData(w, http.StatusConflict, "import_database_taken", "the server already has a database named "+d.Name+", or the name does not fit", map[string]any{"name": d.Name})
			return
		}
		p := dbPlan{name: d.Name, user: d.User, hash: d.PasswordHash}
		// One user per database here: a user shared by several databases keeps only the first.
		if p.user == "" || usedUsers[p.user] || !dbNameRE.MatchString(p.user) {
			if p.user != "" {
				report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "db_user", Name: p.user, Detail: d.Name})
			}
			p.user, p.hash = d.Name, ""
			if usedUsers[p.user] {
				writeErrorData(w, http.StatusConflict, "import_database_taken", "the server already has a database user named "+p.user, map[string]any{"name": p.user})
				return
			}
		}
		if p.hash == "" {
			report.NewPassword = append(report.NewPassword, d.Name)
			p.hash = auth.MariaDBNativeHash(auth.NewToken()[:24])
		}
		usedUsers[p.user] = true
		dbPlans = append(dbPlans, p)
	}

	u := currentUser(r)
	params := tasks.ImportCPanelParams{Server: srv.ID, File: req.File, CPUser: plan.User, Databases: []string{}, Sites: []tasks.ImportSite{}}
	if req.Cron {
		params.Cron = plan.Cron
	}
	for _, d := range domains {
		php := d.PHPVersion
		if v := req.PHP[d.Domain]; v != "" {
			php = v
		}
		if !slices.Contains(agentproto.PHPVersions, php) {
			php = agentproto.DefaultPHPVersion
		}
		if place.Package != nil && len(place.Package.PHPVersions) > 0 && !slices.Contains(place.Package.PHPVersions, php) {
			php = place.Package.PHPVersions[0]
		}
		user := ""
		for range 10 {
			if c := linuxUserFor(d.Domain); func() bool { t, err := s.store.LinuxUserTaken(ctx, c); return err == nil && !t }() {
				user = c
				break
			}
		}
		if user == "" {
			s.internalError(w, r, errors.New("could not find a free Linux account name"))
			return
		}
		site, err := s.store.CreateSite(ctx, store.NewSite{ServerID: srv.ID, Domain: d.Domain, Aliases: d.Aliases, Kind: "php",
			PHPVersion: &php, TLS: "auto", LinuxUser: user, OwnerID: place.OwnerID, CreatedBy: &u.ID})
		if err != nil {
			s.writeSiteStoreError(w, r, err)
			return
		}
		params.Sites = append(params.Sites, tasks.ImportSite{SiteID: site.ID, DocRoot: d.DocRoot, Main: d.Kind == "main"})
		report.Sites = append(report.Sites, d.Domain)
		if d.HasCert {
			report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "certificate", Name: d.Domain})
		}
	}
	for _, d := range dbPlans {
		db, err := s.store.CreateDatabase(ctx, store.NewDatabase{ServerID: srv.ID, Engine: "mariadb", Name: d.name, Username: d.user,
			PasswordHash: d.hash, OwnerID: place.OwnerID, CreatedBy: &u.ID})
		if err != nil {
			s.writeDatabaseStoreError(w, r, err)
			return
		}
		params.Databases = append(params.Databases, db.ID)
	}
	if params.Mail, err = s.makeImportMail(ctx, srv, place, u.ID, mailDomains, &report); err != nil {
		s.writeMailStoreError(w, r, err)
		return
	}
	if err := s.makeImportZones(ctx, place, u.ID, plan, zones, importHosts(domains), &report); err != nil {
		s.writeHostedError(w, r, err)
		return
	}
	if len(report.Zones) > 0 {
		s.queueNameServers(r)
	}
	// Email and zones in the backup that were not chosen.
	for _, d := range plan.Mail {
		if !slices.Contains(req.Mail, d.Domain) && len(d.Mailboxes) > 0 {
			report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "mail", Name: d.Domain, Count: len(d.Mailboxes)})
		}
	}
	for _, z := range plan.Zones {
		if !slices.Contains(req.Zones, z.Name) {
			report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "dns_zone", Name: z.Name})
		}
	}
	for _, c := range plan.CronSettings {
		report.NotImported = append(report.NotImported, tasks.ImportNote{Code: "cron_setting", Name: c})
	}
	params.Report = report
	t, err := s.opts.Tasks.QueueImportCPanel(ctx, params, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "import.cpanel", plan.User, map[string]any{"server_id": srv.ID, "file": req.File, "sites": report.Sites,
		"databases": len(dbPlans), "cron": len(params.Cron), "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, t)
}
