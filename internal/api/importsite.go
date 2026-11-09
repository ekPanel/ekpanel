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

// Importing one website from an archive of its files (any host; aaPanel's site backups),
// with a database dump if it has one (administrators).

func (s *Server) handleImportFiles(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("server_id")
	if !isUUID(id) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return
	}
	if _, err := s.store.GetServer(r.Context(), id); s.serverLookupFailed(w, r, err) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	raw, err := s.hub.Call(ctx, id, agentproto.OpImportList, struct{}{})
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	var res agentproto.ImportListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res.Files)
}

func (s *Server) inspectSiteArchive(ctx context.Context, serverID, archive string, fresh bool) (agentproto.SitePlan, error) {
	key := "site:" + serverID + "/" + archive
	if !fresh {
		if v, ok := s.importPlans.Load(key); ok && time.Since(v.(cachedSitePlan).at) < importPlanTTL {
			return v.(cachedSitePlan).plan, nil
		}
	}
	var plan agentproto.SitePlan
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	raw, err := s.hub.Call(callCtx, serverID, agentproto.OpImportSiteInspect, agentproto.ImportSiteInspectParams{Archive: archive})
	if err != nil {
		return plan, err
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return plan, err
	}
	s.importPlans.Store(key, cachedSitePlan{plan: plan, at: time.Now()})
	return plan, nil
}

type cachedSitePlan struct {
	plan agentproto.SitePlan
	at   time.Time
}

func (s *Server) siteArchiveServer(w http.ResponseWriter, r *http.Request, serverID, archive string) (store.Server, bool) {
	if !isUUID(serverID) {
		writeError(w, http.StatusBadRequest, "server_not_found", "choose a server")
		return store.Server{}, false
	}
	srv, err := s.store.GetServer(r.Context(), serverID)
	if s.serverLookupFailed(w, r, err) {
		return srv, false
	}
	if k := agentproto.ImportFileKind(archive); k != agentproto.ImportKindArchive && k != agentproto.ImportKindZip {
		writeError(w, http.StatusBadRequest, "invalid_site_archive", "choose a .tar.gz, .tar or .zip archive in "+agentproto.ImportDir+" on the server")
		return srv, false
	}
	return srv, true
}

// siteInspection is a website archive with what stands in the way on this panel.
type siteInspection struct {
	agentproto.SitePlan
	// KeepsPassword: WordPress's database password is kept (the hash is not sent).
	KeepsPassword bool     `json:"keeps_password"`
	DomainTaken   bool     `json:"domain_taken"`
	DatabaseTaken bool     `json:"database_taken"`
	PHPVersions   []string `json:"php_versions"`
}

func (s *Server) handleInspectSiteArchive(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ServerID string `json:"server_id"`
		Archive  string `json:"archive"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	srv, ok := s.siteArchiveServer(w, r, req.ServerID, req.Archive)
	if !ok {
		return
	}
	plan, err := s.inspectSiteArchive(r.Context(), srv.ID, req.Archive, true)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	out := siteInspection{SitePlan: plan, PHPVersions: agentproto.PHPVersions}
	if plan.WordPress != nil {
		wp := *plan.WordPress
		out.KeepsPassword = wp.PasswordHash != ""
		wp.PasswordHash = ""
		out.WordPress = &wp
		dbs, err := s.store.ListDatabases(r.Context(), srv.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out.DatabaseTaken = slices.ContainsFunc(dbs, func(x store.Database) bool { return x.Name == wp.DBName || x.Username == wp.DBUser })
	}
	if plan.Domain != "" {
		taken, err := s.store.HostnamesTaken(r.Context(), []string{plan.Domain})
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out.DomainTaken = len(taken) > 0
	}
	writeJSON(w, http.StatusOK, out)
}

type siteImportRequest struct {
	ServerID      string   `json:"server_id"`
	Archive       string   `json:"archive"`
	Domain        string   `json:"domain"`
	Aliases       []string `json:"aliases"`
	Kind          string   `json:"kind"` // php or static
	PHPVersion    string   `json:"php_version"`
	OwnerID       string   `json:"owner_id"`
	Dump          string   `json:"dump"`            // a .sql or .sql.gz in the import folder
	DumpInArchive string   `json:"dump_in_archive"` // or one inside the archive
	// Database is made when set (always with a dump, and for WordPress).
	Database *struct {
		Name string `json:"name"`
		User string `json:"user"`
	} `json:"database"`
}

// siteImportResponse: the task, and the database's new password when one was made (shown
// once, as when a database is created).
type siteImportResponse struct {
	Task             store.Task `json:"task"`
	DatabasePassword string     `json:"database_password,omitempty"`
}

func (s *Server) handleImportSiteArchive(w http.ResponseWriter, r *http.Request) {
	var req siteImportRequest
	if !readJSON(w, r, &req) {
		return
	}
	ctx := r.Context()
	srv, ok := s.siteArchiveServer(w, r, req.ServerID, req.Archive)
	if !ok {
		return
	}
	domain := normalizeHostname(strings.ToLower(req.Domain))
	if domain == "" {
		writeError(w, http.StatusBadRequest, "invalid_domain", "enter a domain such as example.com")
		return
	}
	plan, err := s.inspectSiteArchive(ctx, srv.ID, req.Archive, false)
	if err != nil {
		s.writeAgentError(w, r, err)
		return
	}
	if req.Dump != "" && agentproto.ImportFileKind(req.Dump) != agentproto.ImportKindDump ||
		req.DumpInArchive != "" && !slices.ContainsFunc(plan.Dumps, func(d agentproto.ArchivedDump) bool { return d.Name == req.DumpInArchive }) ||
		req.Dump != "" && req.DumpInArchive != "" {
		writeError(w, http.StatusBadRequest, "invalid_dump", "choose one .sql or .sql.gz dump")
		return
	}
	place, ok := s.resolveOwner(w, r, req.OwnerID)
	if !ok {
		return
	}
	php := req.PHPVersion
	site := siteRequest{Domain: domain, Aliases: req.Aliases, Kind: req.Kind, PHPVersion: &php, TLS: "auto"}
	if site.Kind == "" {
		site.Kind = "php"
	}
	if !validateSiteSettings(w, domain, &site) || !phpAllowed(w, place.Package, &site) {
		return
	}
	wp := plan.Kind == "wordpress" && plan.WordPress != nil
	withDB := req.Database != nil || req.Dump != "" || req.DumpInArchive != ""
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
		if usage.Sites >= pkg.MaxSites {
			writeError(w, http.StatusConflict, "limit_reached_site", "the package does not allow that many websites")
			return
		}
		if withDB && usage.Databases >= pkg.MaxDatabases {
			writeError(w, http.StatusConflict, "limit_reached_database", "the package does not allow that many databases")
			return
		}
	}
	if taken, err := s.store.HostnamesTaken(ctx, append([]string{domain}, site.Aliases...)); err != nil {
		s.internalError(w, r, err)
		return
	} else if len(taken) > 0 {
		writeErrorData(w, http.StatusConflict, "import_domains_taken", "already used here: "+strings.Join(taken, ", "), map[string]any{"names": strings.Join(taken, ", ")})
		return
	}

	// The database: WordPress's own names and password where they fit, so wp-config.php
	// needs no new password.
	var dbName, dbUser, dbHash, dbPassword string
	if withDB {
		if req.Database != nil {
			dbName, dbUser = req.Database.Name, req.Database.User
		} else if wp {
			dbName, dbUser = plan.WordPress.DBName, plan.WordPress.DBUser
		}
		if dbName == "" {
			dbName = strings.ReplaceAll(linuxUserFor(domain), "-", "_")
		}
		if dbUser == "" {
			dbUser = dbName
		}
		if !dbNameRE.MatchString(dbName) || !dbNameRE.MatchString(dbUser) {
			writeError(w, http.StatusBadRequest, "invalid_database_name", "use lowercase letters, digits and _ for the database name and user")
			return
		}
		existing, err := s.store.ListDatabases(ctx, srv.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if slices.ContainsFunc(existing, func(x store.Database) bool { return x.Name == dbName || x.Username == dbUser || x.Username == dbName }) {
			writeErrorData(w, http.StatusConflict, "import_database_taken", "the server already has a database or user named "+dbName,
				map[string]any{"name": dbName})
			return
		}
		if wp && plan.WordPress.PasswordHash != "" {
			dbHash = plan.WordPress.PasswordHash
		} else {
			dbPassword = auth.NewDatabasePassword()
			dbHash = auth.MariaDBNativeHash(dbPassword)
		}
	}

	u := currentUser(r)
	user := ""
	for range 10 {
		if c := linuxUserFor(domain); func() bool { t, err := s.store.LinuxUserTaken(ctx, c); return err == nil && !t }() {
			user = c
			break
		}
	}
	if user == "" {
		s.internalError(w, r, errors.New("could not find a free Linux account name"))
		return
	}
	created, err := s.store.CreateSite(ctx, store.NewSite{ServerID: srv.ID, Domain: domain, Aliases: site.Aliases, Kind: site.Kind,
		PHPVersion: site.PHPVersion, TLS: "auto", LinuxUser: user, OwnerID: place.OwnerID, CreatedBy: &u.ID})
	if err != nil {
		s.writeSiteStoreError(w, r, err)
		return
	}
	params := tasks.ImportSiteParams{Server: srv.ID, SiteID: created.ID, Archive: req.Archive, Root: plan.Root, Dump: req.Dump,
		DumpInArchive: req.DumpInArchive, WordPress: wp && withDB,
		Report: tasks.ImportReport{Sites: []string{domain}, Databases: []string{}, NotImported: []tasks.ImportNote{}}}
	if withDB {
		db, err := s.store.CreateDatabase(ctx, store.NewDatabase{ServerID: srv.ID, Engine: "mariadb", Name: dbName, Username: dbUser,
			PasswordHash: dbHash, OwnerID: place.OwnerID, CreatedBy: &u.ID})
		if err != nil {
			s.writeDatabaseStoreError(w, r, err)
			return
		}
		params.DatabaseID = db.ID // a new password is in the answer, shown once
	}
	if !withDB && len(plan.Dumps) > 0 {
		for _, d := range plan.Dumps {
			params.Report.NotImported = append(params.Report.NotImported, tasks.ImportNote{Code: "dump_unused", Name: d.Name})
		}
	}
	t, err := s.opts.Tasks.QueueImportSite(ctx, params, &u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "import.site", domain, map[string]any{"server_id": srv.ID, "archive": req.Archive, "dump": req.Dump + req.DumpInArchive,
		"database": dbName, "task_id": t.ID})
	writeJSON(w, http.StatusAccepted, siteImportResponse{Task: t, DatabasePassword: dbPassword})
}
