package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

const KindPHPAppInstall = "site.install_app"

type PHPAppOptions struct {
	SiteID     string `json:"site_id"`
	App        string `json:"app"`
	Title      string `json:"title"`
	AdminUser  string `json:"admin_user,omitempty"`
	AdminEmail string `json:"admin_email,omitempty"`
}

// QueuePHPAppInstall starts an install and returns the admin password (empty for apps
// without an admin account), which exists only in the API response and in memory.
func (r *Runner) QueuePHPAppInstall(ctx context.Context, site store.Site, o PHPAppOptions, createdBy *string) (store.Task, string, error) {
	app, _ := agentproto.FindPHPApp(o.App)
	secrets := map[string]string{"db": auth.NewDatabasePassword()}
	if app.Admin {
		secrets["admin"] = auth.NewDatabasePassword()
	}
	t, err := r.store.CreateTask(ctx, store.NewTask{Kind: KindPHPAppInstall, ServerID: &site.ServerID, Params: o, CreatedBy: createdBy, MaxAttempts: 1})
	if err != nil {
		return store.Task{}, "", err
	}
	r.vault.put(t.ID, secrets)
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return t, secrets["admin"], nil
}

func (r *Runner) installPHPApp(ctx context.Context, t store.Task) (any, error) {
	secrets, ok := r.vault.take(t.ID)
	if !ok {
		return nil, errors.New("the panel restarted before this install began, so its one-time passwords are gone; start the install again")
	}
	var o PHPAppOptions
	if err := json.Unmarshal(t.Params, &o); err != nil {
		return nil, err
	}
	app, ok := agentproto.FindPHPApp(o.App)
	if !ok {
		return nil, errors.New("unknown application")
	}
	site, err := r.store.GetSite(ctx, o.SiteID)
	if err != nil {
		return nil, err
	}
	if site.Kind != "php" || site.PHPVersion == nil {
		return nil, errors.New(app.Name + " needs a PHP site")
	}
	// The site serves the app's public folder, with the PHP settings it needs.
	web := site.Web
	web.DocRoot = app.DocRoot
	if len(app.PHP) > 0 {
		web.PHP = maps.Clone(web.PHP)
		if web.PHP == nil {
			web.PHP = map[string]string{}
		}
		maps.Copy(web.PHP, app.PHP)
	}
	if site, err = r.store.SetSiteWeb(ctx, site.ID, web); err != nil {
		return nil, err
	}
	dbName := app.ID[:2] + "_" + site.LinuxUser
	hash := auth.MariaDBNativeHash(secrets["db"])
	db, err := r.store.CreateDatabase(ctx, store.NewDatabase{ServerID: site.ServerID, Engine: "mariadb", Name: dbName, Username: dbName,
		PasswordHash: hash, CreatedBy: t.CreatedBy, OwnerID: site.OwnerID})
	var taken store.ErrDatabaseNameTaken
	if errors.As(err, &taken) {
		if db, err = r.findDatabase(ctx, site.ServerID, dbName); err == nil {
			db, err = r.store.SetDatabasePassword(ctx, db.ID, hash)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("prepare database: %w", err)
	}
	r.Reconcile(ctx, site.ServerID)
	if site, err = r.store.GetSite(ctx, site.ID); err != nil {
		return nil, err
	}
	if db, err = r.store.GetDatabase(ctx, db.ID); err != nil {
		return nil, err
	}
	if site.State != "active" {
		return nil, fmt.Errorf("the website is not ready: %s", deref(site.Message, site.State))
	}
	if db.State != "active" {
		return nil, fmt.Errorf("the database is not ready: %s", deref(db.Message, db.State))
	}
	callCtx, cancel := context.WithTimeout(ctx, lease-5*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, site.ServerID, agentproto.OpPHPAppInstall, agentproto.PHPAppInstallParams{
		App: app.ID, User: site.LinuxUser, Domain: site.Domain, PHPVersion: *site.PHPVersion,
		DB:    agentproto.CloneDB{Name: db.Name, User: db.Username, Password: secrets["db"]},
		Title: o.Title, AdminUser: o.AdminUser, AdminPassword: secrets["admin"], AdminEmail: o.AdminEmail,
	})
	if err != nil {
		return nil, err
	}
	var res agentproto.PHPAppInstallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if err := r.store.SetSiteApp(ctx, site.ID, app.ID, res.Version); err != nil {
		return nil, err
	}
	// Moodle does its background work from a cron job every minute.
	if app.ID == "moodle" {
		_, err := r.store.CreateCronJob(ctx, store.NewCronJob{ServerID: site.ServerID, SiteID: &site.ID, CreatedBy: t.CreatedBy,
			CronSettings: store.CronSettings{Name: "Moodle", Schedule: "* * * * *", Timezone: "UTC", TimeoutSeconds: 3600, Enabled: true,
				Command: "/usr/bin/php" + *site.PHPVersion + " /home/" + site.LinuxUser + "/public_html/admin/cli/cron.php"}})
		if err != nil {
			return nil, fmt.Errorf("add Moodle's cron job: %w", err)
		}
		r.Reconcile(ctx, site.ServerID)
	}
	out := map[string]string{"app": app.Name, "version": res.Version, "url": "https://" + site.Domain + "/", "database": db.Name}
	if app.Admin {
		out["admin_url"] = "https://" + site.Domain + app.AdminPath
		out["admin_user"] = o.AdminUser
	}
	return out, nil
}
