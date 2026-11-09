package agent

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ekpanel/internal/agentproto"
)

// FTP is Pure-FTPd with virtual users: each site with FTP has a login of its own (the
// site's account name and a separate password), mapped to the site's account and locked
// into its home folder. Only TLS sessions are accepted, and Pure-FTPd checks no system
// account, so no other account on the server can sign in over FTP.

const (
	ftpConfDir = "/etc/pure-ftpd/conf"
	ftpAuthDir = "/etc/pure-ftpd/auth"
	ftpPasswd  = "/etc/pure-ftpd/pureftpd.passwd"
	ftpPDB     = "/etc/pure-ftpd/pureftpd.pdb"
	ftpCert    = "/etc/ssl/private/pure-ftpd.pem"
)

// ftpSettings are Pure-FTPd's options, one file each in its conf folder.
var ftpSettings = map[string]string{
	"PureDB":             ftpPDB,
	"PAMAuthentication":  "no",
	"UnixAuthentication": "no",
	"NoAnonymous":        "yes",
	"ChrootEveryone":     "yes",
	"TLS":                "2", // TLS required for sign-in and commands
	"PassivePortRange":   "30000 30100",
	"Umask":              "133 022",
	"DontResolve":        "yes",
	"MaxClientsPerIP":    "10",
	"MaxClientsNumber":   "100",
	"MinUID":             "1000",
}

// applyFTP keeps Pure-FTPd's users to the sites with FTP, and runs it only while there are
// any. A failure is reported on those sites.
func (a *applier) applyFTP(ctx context.Context, sites []agentproto.SiteSpec, passiveIP string) {
	var users []agentproto.SiteSpec
	for _, sp := range sites {
		if sp.FTPEnabled && !sp.Deleted && sp.FTPPasswordHash != "" {
			if r := a.results[sp.ID]; r == nil || r.OK {
				users = append(users, sp)
			}
		}
	}
	installed := func() bool {
		out, _ := a.sys.run(ctx, "dpkg-query", "-W", "-f=${Status}", "pure-ftpd")
		return strings.Contains(string(out), "install ok installed")
	}
	if len(users) == 0 {
		if installed() {
			a.writeIfChanged(ftpPasswd, nil)
			a.sys.run(ctx, "pure-pw", "mkdb", ftpPDB, "-f", ftpPasswd)
			// Its unit comes from a SysV script, where "disable --now" does not stop it.
			a.sys.run(ctx, "systemctl", "disable", "pure-ftpd")
			a.sys.run(ctx, "systemctl", "stop", "pure-ftpd")
		}
		return
	}
	err := func() error {
		if !installed() {
			if err := ensurePackage(ctx, a.sys, "pure-ftpd"); err != nil {
				return fmt.Errorf("install Pure-FTPd: %w", err)
			}
			// The package starts it with system accounts allowed; that stops until set up.
			a.sys.run(ctx, "systemctl", "stop", "pure-ftpd")
		}
		changed, err := a.ftpConfig(passiveIP)
		if err != nil {
			return err
		}
		if _, err := os.Stat(a.sys.path(ftpCert)); err != nil {
			host, _ := os.Hostname()
			if host == "" || strings.ContainsAny(host, "/\\\"=") {
				host = "ftp"
			}
			key, cert := ftpCert+".key.tmp", ftpCert+".crt.tmp"
			defer os.Remove(a.sys.path(key))
			defer os.Remove(a.sys.path(cert))
			if _, err := a.sys.run(ctx, "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "3650", "-subj", "/CN="+host,
				"-keyout", key, "-out", cert); err != nil {
				return fmt.Errorf("make the FTP certificate: %w", err)
			}
			k, err1 := os.ReadFile(a.sys.path(key))
			c, err2 := os.ReadFile(a.sys.path(cert))
			if err1 != nil || err2 != nil {
				return fmt.Errorf("make the FTP certificate: %v %v", err1, err2)
			}
			// Pure-FTPd reads the key and the certificate from one file.
			if err := os.WriteFile(a.sys.path(ftpCert), append(k, c...), 0o600); err != nil {
				return err
			}
			changed = true
		}
		var lines []string
		for _, sp := range users {
			uid, err1 := a.sys.run(ctx, "id", "-u", sp.User)
			gid, err2 := a.sys.run(ctx, "id", "-g", sp.User)
			if err1 != nil || err2 != nil {
				a.fail(sp.ID, fmt.Errorf("FTP: the site's account is missing"))
				continue
			}
			// login:hash:uid:gid:gecos:home with "/./" marking the chroot, then limits left empty.
			lines = append(lines, sp.User+":"+sp.FTPPasswordHash+":"+strings.TrimSpace(string(uid))+":"+strings.TrimSpace(string(gid))+
				"::/home/"+sp.User+"/./::::::::::::")
		}
		sort.Strings(lines)
		content := strings.Join(lines, "\n") + "\n"
		usersChanged, _, err := a.writeIfChanged(ftpPasswd, []byte(content))
		if err != nil {
			return err
		}
		if err := os.Chmod(a.sys.path(ftpPasswd), 0o600); err != nil {
			return err
		}
		if _, err := os.Stat(a.sys.path(ftpPDB)); usersChanged || err != nil {
			if _, err := a.sys.run(ctx, "pure-pw", "mkdb", ftpPDB, "-f", ftpPasswd); err != nil {
				return fmt.Errorf("write the FTP users: %w", err)
			}
		}
		a.sys.run(ctx, "systemctl", "enable", "pure-ftpd")
		if changed || !unitActive(ctx, a.sys, "pure-ftpd") {
			if _, err := a.sys.run(ctx, "systemctl", "restart", "pure-ftpd"); err != nil {
				return fmt.Errorf("start Pure-FTPd: %w", err)
			}
		}
		return nil
	}()
	if err != nil {
		for _, sp := range users {
			a.fail(sp.ID, fmt.Errorf("FTP: %w", err))
		}
	}
}

// ftpConfig writes Pure-FTPd's options and its only way to sign in: the users file.
func (a *applier) ftpConfig(passiveIP string) (bool, error) {
	changed := false
	settings := map[string]string{}
	for k, v := range ftpSettings {
		settings[k] = v
	}
	if ip, err := netip.ParseAddr(passiveIP); err == nil && ip.Is4() {
		settings["ForcePassiveIP"] = ip.String()
	}
	for k, v := range settings {
		c, _, err := a.writeIfChanged(ftpConfDir+"/"+k, []byte(v+"\n"))
		if err != nil {
			return false, err
		}
		changed = changed || c
	}
	if _, ok := settings["ForcePassiveIP"]; !ok && os.Remove(a.sys.path(ftpConfDir+"/ForcePassiveIP")) == nil {
		changed = true
	}
	entries, _ := os.ReadDir(a.sys.path(ftpAuthDir))
	for _, e := range entries {
		if e.Name() != "50puredb" && os.Remove(a.sys.path(filepath.Join(ftpAuthDir, e.Name()))) == nil {
			changed = true
		}
	}
	link := a.sys.path(ftpAuthDir + "/50puredb")
	if target, err := os.Readlink(link); err != nil || target != "../conf/PureDB" {
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return false, err
		}
		os.Remove(link)
		if err := os.Symlink("../conf/PureDB", link); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}
