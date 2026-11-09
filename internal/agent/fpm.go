package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"ekpanel/internal/agentproto"
)

// A customer's processes run in their own systemd slice, which carries the memory and CPU
// limits of their package. PHP-FPM pools of a customer's sites run under a PHP-FPM master of
// their own inside that slice (a pool is a child of its master, so it shares the master's
// cgroup); the administrators' sites use the system's php<version>-fpm.

const (
	customerPHPDir = "/etc/ekpanel/php"
	systemdDir     = "/etc/systemd/system"
)

// ownerKey is a customer ID without dashes, usable in unit names ("-" means nesting there).
func ownerKey(owner string) string { return strings.ReplaceAll(owner, "-", "") }

func customerSlice(key string) string { return "ekpanel-c" + key + ".slice" }

var ownerKeyRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// fpmGroup is one PHP-FPM master: a PHP version, and the customer it belongs to ("" for the
// system's own).
type fpmGroup struct{ version, key string }

func groupFor(sp agentproto.SiteSpec) fpmGroup {
	return fpmGroup{version: sp.PHPVersion, key: ownerKey(sp.Owner)}
}

func (g fpmGroup) unit() string {
	if g.key == "" {
		return "php" + g.version + "-fpm.service"
	}
	return "ekpanel-php" + g.version + "-c" + g.key + ".service"
}

func (g fpmGroup) dir() string { return customerPHPDir + "/c" + g.key + "/" + g.version }

func (g fpmGroup) poolDir() string {
	if g.key == "" {
		return "/etc/php/" + g.version + "/fpm/pool.d"
	}
	return g.dir() + "/pool.d"
}

func (g fpmGroup) poolPath(siteID string) string {
	return g.poolDir() + "/" + poolName(siteID) + ".conf"
}

// socket differs per master: while a site moves between masters (another PHP version), the
// old one removes its socket on reload and must not remove the new one's.
func (g fpmGroup) socket(siteID string) string {
	if g.key == "" {
		return socketPath(siteID, g.version)
	}
	return "/run/php/ekpanel-" + siteID + "-php" + g.version + "-c" + g.key[:8] + ".sock"
}

// poolGroups finds every pool file of a site (or of all sites, with siteID "*"), in the
// system's masters and the customers'.
func (a *applier) poolFiles(siteID string) map[string]fpmGroup {
	out := map[string]fpmGroup{}
	for _, v := range agentproto.PHPVersions {
		sys := fpmGroup{version: v}
		m, _ := filepath.Glob(a.sys.path(sys.poolDir() + "/" + poolName(siteID) + ".conf"))
		for _, p := range m {
			out[p] = sys
		}
		m, _ = filepath.Glob(a.sys.path(customerPHPDir + "/c*/" + v + "/pool.d/" + poolName(siteID) + ".conf"))
		for _, p := range m {
			key := strings.TrimPrefix(filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(p)))), "c")
			if ownerKeyRE.MatchString(key) {
				out[p] = fpmGroup{version: v, key: key}
			}
		}
	}
	return out
}

// removePool deletes a pool file and notes that its master lost a pool.
func (a *applier) removePool(path string, g fpmGroup) {
	if err := os.Remove(path); err == nil {
		if _, gained := a.reloadPHP[g]; !gained {
			a.reloadPHP[g] = false
		}
	}
}

var masterTemplate = template.Must(template.New("master").Parse(`; Managed by ekPanel (customer {{.Key}}, PHP {{.Version}}). Changes here are overwritten.
[global]
error_log = syslog
syslog.ident = ekpanel-php{{.Version}}-c{{.Key}}
daemonize = no
include = {{.PoolDir}}/*.conf
`))

var unitTemplate = template.Must(template.New("unit").Parse(`# Managed by ekPanel. Changes here are overwritten.
[Unit]
Description=PHP {{.Version}} FastCGI for an ekPanel customer
After=network.target

[Service]
Type=notify
Slice={{.Slice}}
ExecStart=/usr/sbin/php-fpm{{.Version}} --nodaemonize --fpm-config {{.Config}}
ExecReload=/bin/kill -USR2 $MAINPID
Restart=on-failure
# A script over the memory limit costs only its own worker; the master and the
# customer's other sites keep running.
OOMPolicy=continue

[Install]
WantedBy=multi-user.target
`))

// prepareMaster writes a customer master's configuration and unit. It reports whether the
// unit file changed, which needs a daemon-reload.
func (a *applier) prepareMaster(g fpmGroup) (bool, error) {
	if g.key == "" {
		return false, nil
	}
	vars := map[string]string{"Key": g.key, "Version": g.version, "PoolDir": g.poolDir(), "Slice": customerSlice(g.key), "Config": g.dir() + "/php-fpm.conf"}
	var conf, unit bytes.Buffer
	masterTemplate.Execute(&conf, vars)
	unitTemplate.Execute(&unit, vars)
	if _, _, err := a.writeIfChanged(g.dir()+"/php-fpm.conf", conf.Bytes()); err != nil {
		return false, err
	}
	changed, _, err := a.writeIfChanged(systemdDir+"/"+g.unit(), unit.Bytes())
	return changed, err
}

// retireMaster stops a customer master that has no pools left.
func (a *applier) retireMaster(ctx context.Context, g fpmGroup) bool {
	if g.key == "" {
		return false
	}
	if pools, _ := filepath.Glob(a.sys.path(g.poolDir() + "/*.conf")); len(pools) > 0 {
		return false
	}
	a.sys.run(ctx, "systemctl", "disable", "--now", g.unit())
	os.Remove(a.sys.path(systemdDir + "/" + g.unit()))
	os.RemoveAll(a.sys.path(g.dir()))
	return true
}

// ---- Customer slices ----

var sliceTemplate = template.Must(template.New("slice").Parse(`# Managed by ekPanel (customer {{.Owner}}). Changes here are overwritten.
[Unit]
Description=ekPanel customer {{.Owner}}

[Slice]
MemoryAccounting=yes
CPUAccounting=yes
TasksAccounting=yes
MemoryMax={{.Memory}}
CPUQuota={{.CPU}}
TasksMax=4096
`))

// applyOwners writes one slice per customer and applies changed limits at once. Slices of
// customers the panel no longer sends are removed (their processes are gone with them).
func (a *applier) applyOwners(ctx context.Context, owners []agentproto.OwnerSpec) error {
	known := map[string]bool{}
	reload := false
	type limits struct{ slice, memory, cpu string }
	var changed []limits
	for _, o := range owners {
		if !uuidRE.MatchString(o.ID) || o.MemoryMB < 0 || o.CPUPercent < 0 {
			return fmt.Errorf("invalid customer limits for %q", o.ID)
		}
		key := ownerKey(o.ID)
		known[key] = true
		l := limits{slice: customerSlice(key), memory: "infinity"}
		if o.MemoryMB > 0 {
			l.memory = strconv.Itoa(o.MemoryMB) + "M"
		}
		if o.CPUPercent > 0 {
			l.cpu = strconv.Itoa(o.CPUPercent) + "%"
		}
		var buf bytes.Buffer
		sliceTemplate.Execute(&buf, map[string]string{"Owner": o.ID, "Memory": l.memory, "CPU": l.cpu})
		c, _, err := a.writeIfChanged(systemdDir+"/"+l.slice, buf.Bytes())
		if err != nil {
			return err
		}
		if c {
			reload = true
			changed = append(changed, l)
		}
	}
	entries, _ := filepath.Glob(a.sys.path(systemdDir + "/ekpanel-c*.slice"))
	for _, p := range entries {
		key := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "ekpanel-c"), ".slice")
		if ownerKeyRE.MatchString(key) && !known[key] && os.Remove(p) == nil {
			reload = true
		}
	}
	if !reload {
		return nil
	}
	if _, err := a.sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	// The unit file applies when a slice starts; a running slice gets the new limits now,
	// without stopping what runs in it.
	for _, l := range changed {
		if _, err := a.sys.run(ctx, "systemctl", "set-property", "--runtime", l.slice, "MemoryMax="+l.memory, "CPUQuota="+l.cpu); err != nil {
			return err
		}
	}
	return nil
}
