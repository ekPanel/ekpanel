package agent

import (
	"context"
	"slices"
	"strings"

	"ekpanel/internal/agentproto"
)

// installedPHPPackages returns the installed php<version>-* packages, by extension name.
func installedPHPPackages(ctx context.Context, sys *system, version string) map[string]bool {
	out, _ := sys.run(ctx, "dpkg-query", "-W", "-f=${Package} ${Status}\n", "php"+version+"-*")
	installed := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		pkg, status, _ := strings.Cut(line, " ")
		if name, ok := strings.CutPrefix(pkg, "php"+version+"-"); ok && strings.Contains(status, "install ok installed") {
			installed[name] = true
		}
	}
	return installed
}

// phpInfo lists the PHP versions that are installed (their FPM is) and their extensions.
func phpInfo(ctx context.Context, sys *system) (*agentproto.PHPInfoResult, *agentproto.OpError) {
	res := &agentproto.PHPInfoResult{Versions: []agentproto.PHPVersionInfo{}}
	for _, v := range agentproto.PHPVersions {
		installed := installedPHPPackages(ctx, sys, v)
		if !installed["fpm"] {
			continue
		}
		info := agentproto.PHPVersionInfo{Version: v}
		for _, e := range agentproto.RequiredPHPExtensions {
			info.Extensions = append(info.Extensions, agentproto.PHPExtensionInfo{Name: e, Installed: installed[e], Required: true})
		}
		for _, e := range agentproto.OptionalPHPExtensions {
			info.Extensions = append(info.Extensions, agentproto.PHPExtensionInfo{Name: e, Installed: installed[e]})
		}
		res.Versions = append(res.Versions, info)
	}
	return res, nil
}

// phpExtension installs or removes an optional extension, then reloads every PHP-FPM
// master of that version (the system's and customers') so their workers load it.
func phpExtension(ctx context.Context, sys *system, p agentproto.PHPExtensionParams) (any, *agentproto.OpError) {
	if !slices.Contains(agentproto.PHPVersions, p.Version) || !slices.Contains(agentproto.OptionalPHPExtensions, p.Name) {
		return nil, opError(agentproto.ErrInvalidParams, "unknown PHP version or extension")
	}
	if !installedPHPPackages(ctx, sys, p.Version)["fpm"] {
		return nil, opError(agentproto.ErrNotAllowed, "PHP %s is not installed on this server", p.Version)
	}
	pkg := "php" + p.Version + "-" + p.Name
	action := "remove"
	if p.Installed {
		action = "install"
	}
	if _, err := sys.run(ctx, "apt-get", "-o", "DPkg::Lock::Timeout=300", action, "-y", "--no-install-recommends", pkg); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	out, _ := sys.run(ctx, "systemctl", "list-units", "--type=service", "--state=active", "--plain", "--no-legend",
		"php"+p.Version+"-fpm.service", "ekpanel-php"+p.Version+"-c*.service")
	for _, line := range strings.Split(string(out), "\n") {
		if unit := strings.Fields(line); len(unit) > 0 {
			if _, err := sys.run(ctx, "systemctl", "reload-or-restart", unit[0]); err != nil {
				return nil, opError(agentproto.ErrFailed, "%v", err)
			}
		}
	}
	return struct{}{}, nil
}
