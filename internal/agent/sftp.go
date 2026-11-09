package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"ekpanel/internal/agentproto"
)

func normalizeSSHKey(line string) (string, error) { return agentproto.NormalizeSSHKey(line) }

// sftpGroup lets a site's account sign in over SFTP. Membership is the only switch:
// sshd locks members into their home folder and allows nothing but file transfer.
const sftpGroup = "ekpanel-sftp"

const (
	sshdDropIn  = "/etc/ssh/sshd_config.d/ekpanel-sftp.conf"
	sshKeysDir  = "/etc/ekpanel/ssh"
	maxSSHKeys  = 20
	shadowPath  = "/etc/shadow"
	noPassword  = "*" // shadow value that matches no password; key sign-in still works
	sshdService = "ssh"
)

var cryptHashRE = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./A-Za-z0-9]{1,16}\$[./A-Za-z0-9]{86}$`)

// The keys file sits outside the chroot and belongs to root, so a site's account cannot
// add keys for itself.
const sshdConfig = `# Managed by ekPanel. Website accounts in the ekpanel-sftp group may use SFTP only,
# locked into their own home folder.
Match Group ekpanel-sftp
	ChrootDirectory %h
	ForceCommand internal-sftp -u 0022
	AuthorizedKeysFile /etc/ekpanel/ssh/%u
	PasswordAuthentication yes
	AllowTcpForwarding no
	AllowAgentForwarding no
	AllowStreamLocalForwarding no
	PermitTunnel no
	X11Forwarding no
	PermitTTY no
# End the Match block, so settings after this file are not limited to the group.
Match all
`

// ensureSSHD installs the SFTP rules once per run. sshd checks them before reloading; if
// they are rejected the file is removed again, so a mistake can never lock out SSH.
func (a *applier) ensureSSHD(ctx context.Context) error {
	if a.sshdReady != nil {
		return *a.sshdReady
	}
	err := func() error {
		if _, err := os.Stat(a.sys.path("/usr/sbin/sshd")); err != nil {
			return errors.New("the OpenSSH server is not installed on this server (apt install openssh-server)")
		}
		if err := os.MkdirAll(a.sys.path(sshKeysDir), 0o755); err != nil {
			return err
		}
		changed, prev, err := a.writeIfChanged(sshdDropIn, []byte(sshdConfig))
		if err != nil || !changed {
			return err
		}
		if _, err := a.sys.run(ctx, "sshd", "-t"); err != nil {
			if prev == nil {
				os.Remove(a.sys.path(sshdDropIn))
			} else {
				os.WriteFile(a.sys.path(sshdDropIn), prev, 0o644)
			}
			return fmt.Errorf("sshd rejected the SFTP settings: %w", err)
		}
		// try-reload: if sshd is socket-activated and idle, it reads the file when it next starts.
		_, err = a.sys.run(ctx, "systemctl", "try-reload-or-restart", sshdService)
		return err
	}()
	a.sshdReady = &err
	return err
}

// shadowHash returns the current password field of an account.
func (a *applier) shadowHash(user string) string {
	b, _ := os.ReadFile(a.sys.path(shadowPath))
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) >= 2 && f[0] == user {
			return f[1]
		}
	}
	return ""
}

func (a *applier) ensureSFTP(ctx context.Context, sp agentproto.SiteSpec) error {
	out, _ := a.sys.run(ctx, "id", "-nG", sp.User)
	member := slices.Contains(strings.Fields(string(out)), sftpGroup)
	keysFile := sshKeysDir + "/" + sp.User

	if !sp.SFTPEnabled {
		if member {
			if _, err := a.sys.run(ctx, "gpasswd", "-d", sp.User, sftpGroup); err != nil {
				return err
			}
		}
		os.Remove(a.sys.path(keysFile))
		return a.setPasswordHash(ctx, sp.User, noPassword)
	}

	if err := a.ensureSSHD(ctx); err != nil {
		return err
	}
	var keys bytes.Buffer
	for _, k := range sp.SSHKeys {
		n, _ := normalizeSSHKey(k) // validated earlier
		keys.WriteString(n + "\n")
	}
	if keys.Len() == 0 {
		os.Remove(a.sys.path(keysFile))
	} else if _, _, err := a.writeIfChanged(keysFile, keys.Bytes()); err != nil {
		return err
	}
	hash := sp.SFTPPasswordHash
	if hash == "" {
		hash = noPassword
	}
	if err := a.setPasswordHash(ctx, sp.User, hash); err != nil {
		return err
	}
	if !member {
		if _, err := a.sys.run(ctx, "usermod", "-aG", sftpGroup, sp.User); err != nil {
			return err
		}
	}
	return nil
}

// setPasswordHash writes a crypt hash to /etc/shadow through chpasswd's stdin, so it never
// appears in the process list. Unchanged hashes are left alone.
func (a *applier) setPasswordHash(ctx context.Context, user, hash string) error {
	if a.shadowHash(user) == hash {
		return nil
	}
	_, err := a.sys.runInput(ctx, user+":"+hash+"\n", "chpasswd", "--encrypted")
	return err
}
