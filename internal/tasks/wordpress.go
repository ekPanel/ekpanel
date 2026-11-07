package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/auth"
	"ekpanel/internal/store"
)

type WordPressOptions struct {
	SiteID     string `json:"site_id"`
	Title      string `json:"title"`
	AdminUser  string `json:"admin_user"`
	AdminEmail string `json:"admin_email"`
	Locale     string `json:"locale"`
}

// QueueWordPressInstall starts a WordPress install and returns the new admin password,
// which exists only in the API response and in memory until the task uses it.
func (r *Runner) QueueWordPressInstall(ctx context.Context, site store.Site, opts WordPressOptions, createdBy *string) (store.Task, string, error) {
	adminPassword := auth.NewDatabasePassword()
	t, err := r.store.CreateTask(ctx, store.NewTask{
		Kind: KindWordPressInstall, ServerID: &site.ServerID, Params: opts, CreatedBy: createdBy, MaxAttempts: 1,
	})
	if err != nil {
		return store.Task{}, "", err
	}
	// Secrets go in before any worker is woken, so the task never starts without them.
	r.vault.put(t.ID, map[string]string{"admin": adminPassword, "db": auth.NewDatabasePassword()})
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return t, adminPassword, nil
}

type WordPressResult struct {
	Version   string `json:"version"`
	AdminURL  string `json:"admin_url"`
	AdminUser string `json:"admin_user"`
	Database  string `json:"database"`
}

func (r *Runner) installWordPress(ctx context.Context, t store.Task) (any, error) {
	secrets, ok := r.vault.take(t.ID)
	if !ok {
		return nil, errors.New("the panel restarted before this install began, so its one-time passwords are gone; start the install again")
	}
	var o WordPressOptions
	if err := json.Unmarshal(t.Params, &o); err != nil {
		return nil, err
	}
	site, err := r.store.GetSite(ctx, o.SiteID)
	if err != nil {
		return nil, err
	}
	if site.Kind != "php" || site.PHPVersion == nil {
		return nil, errors.New("WordPress needs a PHP site")
	}

	// The site's own MariaDB database: wp_<site account>. A previous failed attempt may
	// have created it already; then it just gets the new password.
	dbName := "wp_" + site.LinuxUser
	hash := auth.MariaDBNativeHash(secrets["db"])
	db, err := r.store.CreateDatabase(ctx, store.NewDatabase{
		ServerID: site.ServerID, Engine: "mariadb", Name: dbName, Username: dbName, PasswordHash: hash, CreatedBy: t.CreatedBy,
	})
	var taken store.ErrDatabaseNameTaken
	if errors.As(err, &taken) {
		db, err = r.findDatabase(ctx, site.ServerID, dbName)
		if err == nil {
			db, err = r.store.SetDatabasePassword(ctx, db.ID, hash)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("prepare database: %w", err)
	}

	// Apply the site and database on the server, then check both are ready.
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

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	raw, err := r.hub.Call(callCtx, site.ServerID, agentproto.OpWordPressInstall, agentproto.WordPressInstallParams{
		User: site.LinuxUser, Domain: site.Domain, PHPVersion: *site.PHPVersion,
		DBName: db.Name, DBUser: db.Username, DBPassword: secrets["db"],
		Title: o.Title, AdminUser: o.AdminUser, AdminPassword: secrets["admin"], AdminEmail: o.AdminEmail, Locale: o.Locale,
	})
	if err != nil {
		return nil, err
	}
	var res agentproto.WordPressInstallResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if err := r.store.SetSiteApp(ctx, site.ID, "wordpress", res.Version); err != nil {
		return nil, err
	}
	return WordPressResult{Version: res.Version, AdminURL: "https://" + site.Domain + "/wp-admin/", AdminUser: o.AdminUser, Database: db.Name}, nil
}

func (r *Runner) findDatabase(ctx context.Context, serverID, name string) (store.Database, error) {
	list, err := r.store.ListDatabases(ctx, serverID)
	if err != nil {
		return store.Database{}, err
	}
	for _, d := range list {
		if d.Engine == "mariadb" && d.Name == name {
			return d, nil
		}
	}
	return store.Database{}, store.ErrNotFound
}

func deref(s *string, fallback string) string {
	if s != nil && *s != "" {
		return *s
	}
	return fallback
}
