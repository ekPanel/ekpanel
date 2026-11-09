package cli

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
)

// commands is the command tree.
func (a *app) commands() *command {
	root := &command{
		name:    "ekctl",
		summary: "ekctl manages an ekPanel panel from the command line",
		long: "ekctl manages an ekPanel panel from the command line.\n\n" +
			"Start with: ekctl login --url https://panel.example.com\n" +
			"Create the API token it asks for in the panel, under Account → API tokens.",
	}
	root.subs = []*command{
		a.loginCmd(), a.logoutCmd(), a.whoamiCmd(),
		a.serversCmd(), a.sitesCmd(), a.databasesCmd(), a.appsCmd(), a.dnsCmd(), a.backupsCmd(), a.tasksCmd(),
		a.apiCmd(), a.versionCmd(), a.helpCmd(root),
	}
	return link(root)
}

func (a *app) loginCmd() *command {
	return &command{
		name:    "login",
		summary: "Log in to a panel with an API token",
		long: "Log in to a panel with an API token and save it.\n\n" +
			"Create the token in the panel, under Account → API tokens. Without --token, ekctl reads it\n" +
			"from stdin (hidden when you type it). The token and URL are saved in the config file, which\n" +
			"only you can read.\n\n" +
			"If the panel uses a certificate your system does not trust (a self-signed one), ekctl shows\n" +
			"its SHA-256 fingerprint and asks whether to trust it. Compare it with the fingerprint your\n" +
			"browser shows for the panel. Without a terminal, pass the fingerprint with --fingerprint.\n" +
			"After that, ekctl accepts exactly that certificate for this panel.",
		usage: "--url URL [--token TOKEN] [--fingerprint SHA256]",
		examples: []string{
			"ekctl login --url https://panel.example.com",
			"echo \"$EKP_TOKEN\" | ekctl login --url https://203.0.113.7:8443 --fingerprint AB:CD:…",
		},
		run: func(fs *flag.FlagSet) func([]string) error {
			fp := fs.String("fingerprint", "", "trust the panel's certificate if its SHA-256 fingerprint is `SHA256`")
			return func([]string) error { return a.login(*fp) }
		},
	}
}

func (a *app) login(fpFlag string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	raw := a.g.URL
	if raw == "" {
		raw = a.env.Getenv("EKCTL_URL")
	}
	if raw == "" {
		raw = cfg.URL
	}
	if raw == "" {
		return usagef("Missing --url. Example: ekctl login --url https://panel.example.com")
	}
	base, err := normalizeURL(raw)
	if err != nil {
		return usagef("%v", err)
	}
	want := ""
	if fpFlag != "" {
		if want, err = normalizeFingerprint(fpFlag); err != nil {
			return err
		}
	}
	if strings.HasPrefix(base, "http://") {
		fmt.Fprintf(a.env.Stderr, "Warning: %s does not use HTTPS. The token is sent unencrypted.\n", base)
	}
	hp := hostPort(base)

	// First the connection: is this a panel, and is its certificate trusted?
	version, pin, err := a.checkPanel(base, cfg.TrustedCerts[hp], want)
	if err != nil {
		return err
	}

	token := a.g.Token
	if token == "" {
		if a.env.StdinIsTerminal {
			token, err = a.readSecret("API token (create one in the panel under Account → API tokens): ")
		} else {
			token, err = a.readLine()
		}
		if err != nil {
			if a.ctx.Err() != nil {
				return a.ctx.Err()
			}
			return usagef("No token was given. Pass it with --token, or on stdin: echo \"$TOKEN\" | ekctl login --url %s", base)
		}
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "ekp_") || len(token) < 10 {
		return usagef("That is not an ekPanel API token. Tokens start with ekp_. Create one in the panel under Account → API tokens.")
	}

	c := a.newClient(base, token, pin)
	var me userInfo
	noRead := false
	if _, err := c.get(a.ctx, "auth/me", &me); err != nil {
		var ae *APIError
		switch {
		case errors.As(err, &ae) && ae.Status == 401:
			return fmt.Errorf("The panel did not accept this token: %s. Check that you copied all of it.", ae.Message)
		case errors.As(err, &ae) && ae.Code == "token_scope":
			// The token is valid, but the panel would not say whose it is (panels before
			// v0.2 asked for the "read" scope for this).
			noRead = true
		default:
			return err
		}
	}

	cfg.URL, cfg.Token = base, token
	if pin != "" {
		cfg.TrustedCerts[hp] = pin
	}
	if err := a.saveConfig(); err != nil {
		return err
	}
	if a.g.JSON {
		v := map[string]any{"url": base, "version": version, "user": me}
		if noRead {
			v["user"] = nil
		}
		return a.printValue(v)
	}
	if noRead {
		fmt.Fprintf(a.env.Stdout, "Logged in to %s.\n", base)
		fmt.Fprintln(a.env.Stderr, "The panel did not say whose token this is; it may be an older version. Commands still work within the token's scopes.")
	} else {
		fmt.Fprintf(a.env.Stdout, "Logged in to %s as %s <%s> (%s).\n", base, me.Name, me.Email, me.Role)
	}
	fmt.Fprintf(a.env.Stderr, "Saved in %s.\n", a.cfgPath)
	return nil
}

type userInfo struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// checkPanel connects to the panel's /health. When its certificate is not trusted it asks
// whether to trust it, and returns the fingerprint to pin.
func (a *app) checkPanel(base, pin, want string) (version, newPin string, err error) {
	for range 3 {
		var h struct {
			Status  string `json:"status"`
			Version string `json:"version"`
		}
		_, err = a.newClient(base, "", pin).get(a.ctx, "health", &h)
		if err == nil {
			if h.Status != "ok" {
				return "", "", fmt.Errorf("%s does not look like an ekPanel panel. Check the URL.", base)
			}
			return h.Version, pin, nil
		}
		var ae *APIError
		if errors.As(err, &ae) {
			return "", "", fmt.Errorf("%s does not look like an ekPanel panel (%s). Check the URL.", base, ae.Message)
		}
		cert, reason, changed := untrustedCert(err)
		if cert == nil {
			return "", "", err
		}
		fp := fingerprint(cert.Raw)
		if err := a.decideTrust(base, cert, fp, reason, changed, want); err != nil {
			return "", "", err
		}
		pin = fp
	}
	return "", "", err
}

func (a *app) decideTrust(base string, cert *x509.Certificate, fp, reason string, changed bool, want string) error {
	w := a.env.Stderr
	if changed {
		fmt.Fprintf(w, "WARNING: the certificate of %s has changed since you trusted it.\n"+
			"Someone may be intercepting the connection, or the panel got a new certificate.\n", base)
	} else {
		fmt.Fprintf(w, "The certificate of %s is not trusted: %s.\n"+
			"This is normal for a panel with a self-signed certificate.\n", base, reason)
	}
	fmt.Fprintf(w, "  Subject:  %s\n  Issuer:   %s\n  Valid:    %s to %s\n  SHA-256:  %s\n",
		cert.Subject.String(), cert.Issuer.String(), fmtTime(cert.NotBefore), fmtTime(cert.NotAfter), fp)
	if want != "" {
		if want == fp {
			fmt.Fprintln(w, "It matches --fingerprint. Trusting it.")
			return nil
		}
		return fmt.Errorf("The certificate's fingerprint is %s, not the %s given with --fingerprint. Not trusting it.", fp, want)
	}
	if !a.env.StdinIsTerminal {
		return fmt.Errorf("Not trusting the certificate. Compare the fingerprint above with the one your browser shows for the panel. If they match, run again with --fingerprint %s", fp)
	}
	fmt.Fprint(w, "Compare it with the fingerprint your browser shows for the panel.\nTrust this certificate? [y/N] ")
	line, err := a.readLine()
	if err != nil && a.ctx.Err() != nil {
		return a.ctx.Err()
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	}
	return errors.New("Not trusting the certificate. Nothing was saved.")
}

func (a *app) logoutCmd() *command {
	return &command{
		name:    "logout",
		summary: "Forget the saved token",
		long: "Forget the saved token. The panel URL and trusted certificates stay saved.\n\n" +
			"The token itself still works until you revoke it in the panel (Account → API tokens).",
		run: func(fs *flag.FlagSet) func([]string) error {
			return func([]string) error {
				cfg, err := a.config()
				if err != nil {
					return err
				}
				if cfg.Token == "" {
					fmt.Fprintln(a.env.Stderr, "You are not logged in.")
					return nil
				}
				cfg.Token = ""
				if err := a.saveConfig(); err != nil {
					return err
				}
				fmt.Fprintf(a.env.Stdout, "Logged out of %s.\n", cfg.URL)
				fmt.Fprintln(a.env.Stderr, "The token still works until you revoke it in the panel (Account → API tokens).")
				return nil
			}
		},
	}
}

func (a *app) whoamiCmd() *command {
	return &command{
		name:    "whoami",
		summary: "Show who you are logged in as, and the panel's version",
		run: func(fs *flag.FlagSet) func([]string) error {
			return func([]string) error {
				c, err := a.client()
				if err != nil {
					return err
				}
				var me userInfo
				rawMe, err := c.get(a.ctx, "auth/me", &me)
				if err != nil {
					return err
				}
				var h struct {
					Version string `json:"version"`
				}
				if _, err := c.get(a.ctx, "health", &h); err != nil {
					return err
				}
				if a.g.JSON {
					return a.printValue(map[string]any{
						"user":  json.RawMessage(rawMe),
						"panel": map[string]string{"url": c.base, "version": h.Version},
					})
				}
				return a.details([][2]string{
					{"Name", me.Name}, {"Email", me.Email}, {"Role", me.Role},
					{"Panel", c.base}, {"Version", h.Version},
				})
			}
		},
	}
}

func (a *app) versionCmd() *command {
	return &command{
		name:    "version",
		summary: "Show ekctl's version",
		run: func(fs *flag.FlagSet) func([]string) error {
			return func([]string) error { return a.printVersion() }
		},
	}
}

func (a *app) helpCmd(root *command) *command {
	return &command{
		name:    "help",
		summary: "Show help for a command",
		usage:   "[command …]",
		anyArgs: true,
		run: func(fs *flag.FlagSet) func([]string) error {
			return func(args []string) error {
				c := root
				for _, name := range args {
					sub := c.find(name)
					if sub == nil {
						return usagef("Unknown command %q for %q.", name, c.fullName())
					}
					c = sub
				}
				return a.printHelp(c)
			}
		},
	}
}
