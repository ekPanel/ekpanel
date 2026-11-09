package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// watchedServices are the systemd units whose state the panel shows. Only names from
// these fixed lists ever reach systemctl, so no input from the panel becomes part of a command.
var watchedServices = []string{"ekpanel", "ekpanel-agent", "caddy", "postgresql", "ssh", "cron", "ekpanel-mail", "crowdsec", "crowdsec-firewall-bouncer"}

// restartableServices may be restarted from the panel. The panel and agent themselves are
// not on it (a restart would cut the connection running the request), nor is ssh (a bad
// restart could lock the owner out).
var restartableServices = []string{"caddy", "postgresql", "cron", "ekpanel-mail", "crowdsec", "crowdsec-firewall-bouncer"}

// systemctl runs systemctl with fixed arguments. Tests replace it.
var systemctl = func(ctx context.Context, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// systemdError classifies a failed systemctl call.
func systemdError(err error) *agentproto.OpError {
	msg := err.Error()
	if strings.Contains(msg, "not been booted with systemd") || strings.Contains(msg, "Failed to connect to bus") {
		return opError(agentproto.ErrNoSystemd, "this server does not run systemd, so services cannot be managed")
	}
	return opError(agentproto.ErrFailed, "%s", msg)
}

func opError(code, format string, a ...any) *agentproto.OpError {
	return &agentproto.OpError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// Dispatch runs one operation requested by the panel.
func Dispatch(ctx context.Context, op string, params json.RawMessage) (any, *agentproto.OpError) {
	switch op {
	case agentproto.OpServicesStatus:
		return servicesStatus(ctx)
	case agentproto.OpStateApply:
		var p agentproto.StateApplyParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return stateApply(ctx, realSystem, p)
	case agentproto.OpPHPInfo:
		return phpInfo(ctx, realSystem)
	case agentproto.OpPHPExtension:
		var p agentproto.PHPExtensionParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return phpExtension(ctx, realSystem, p)
	case agentproto.OpFiles:
		var p agentproto.FilesParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return runFiles(ctx, realSystem, p)
	case agentproto.OpWordPressInstall:
		var p agentproto.WordPressInstallParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return installWordPress(ctx, realSystem, p)
	case agentproto.OpBackupSetup:
		var p agentproto.BackupSetupParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return backupSetup(ctx, realSystem, p)
	case agentproto.OpBackupRun:
		var p agentproto.BackupRunParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return backupRun(ctx, realSystem, p)
	case agentproto.OpBackupSnapshots:
		var p agentproto.BackupSnapshotsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return backupSnapshots(ctx, realSystem, p)
	case agentproto.OpBackupRestore:
		var p agentproto.BackupRestoreParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return backupRestore(ctx, realSystem, p)
	case agentproto.OpBackupRemove:
		var p agentproto.BackupRemoveParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return backupRemove(realSystem, p)
	case agentproto.OpAppStatus, agentproto.OpAppLogs, agentproto.OpAppRestart, agentproto.OpAppPull:
		var p agentproto.AppParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		switch op {
		case agentproto.OpAppStatus:
			return appStatus(ctx, realSystem, p)
		case agentproto.OpAppLogs:
			return appLogs(ctx, realSystem, p)
		case agentproto.OpAppRestart:
			return appRestart(ctx, realSystem, p)
		default:
			return appPull(ctx, realSystem, p)
		}
	case agentproto.OpCronRun, agentproto.OpCronStatus:
		var p agentproto.CronParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		if op == agentproto.OpCronRun {
			return cronRun(ctx, realSystem, p)
		}
		return cronStatus(ctx, realSystem, p)
	case agentproto.OpAccessLog:
		return accessLogOp(ctx, realSystem, params)
	case agentproto.OpTraffic:
		return trafficOp(ctx, realSystem, params)
	case agentproto.OpDiskUsage:
		var p agentproto.DiskUsageParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return diskUsage(ctx, realSystem, p)
	case agentproto.OpWordPressInfo:
		var p agentproto.WordPressInfoParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return wordPressInfo(ctx, realSystem, p)
	case agentproto.OpWordPressUpdate:
		var p agentproto.WordPressUpdateParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return wordPressUpdate(ctx, realSystem, p)
	case agentproto.OpWordPressManage:
		var p agentproto.WordPressManageParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return wordPressManage(ctx, realSystem, p)
	case agentproto.OpWordPressClone:
		var p agentproto.WordPressCloneParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return wordPressClone(ctx, realSystem, p)
	case agentproto.OpSecurityOverview:
		return securityOverview(ctx, realSystem)
	case agentproto.OpSecurityBans:
		return securityBans(ctx, realSystem)
	case agentproto.OpSecurityAlerts:
		return securityAlerts(ctx, realSystem)
	case agentproto.OpSecurityBan:
		var p agentproto.BanParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return securityBan(ctx, realSystem, p)
	case agentproto.OpSecurityUnban:
		var p agentproto.UnbanParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return securityUnban(ctx, realSystem, p)
	case agentproto.OpSecurityAutoUpdates:
		var p agentproto.AutoUpdatesParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return securityAutoUpdates(ctx, realSystem, p)
	case agentproto.OpCountryOf:
		var p agentproto.CountryOfParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return securityCountryOf(ctx, realSystem, p)
	case agentproto.OpMalwareScan:
		var p agentproto.MalwareScanParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return malwareScan(ctx, realSystem, p)
	case agentproto.OpMalwareQuarantine, agentproto.OpMalwareRestore, agentproto.OpMalwareDelete:
		var p agentproto.MalwareFileParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		switch op {
		case agentproto.OpMalwareQuarantine:
			return malwareQuarantine(ctx, realSystem, p)
		case agentproto.OpMalwareRestore:
			return malwareRestore(ctx, realSystem, p)
		}
		return malwareDelete(ctx, realSystem, p)
	case agentproto.OpPHPAppInstall:
		var p agentproto.PHPAppInstallParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return installPHPApp(ctx, realSystem, p)
	case agentproto.OpMailCheck:
		var p agentproto.MailCheckParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return mailCheck(ctx, p)
	case agentproto.OpServicesRestart:
		var p agentproto.ServicesRestartParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, opError(agentproto.ErrInvalidParams, "%v", err)
		}
		return servicesRestart(ctx, p.Name)
	default:
		return nil, opError(agentproto.ErrUnknownOp, "this agent does not know %q; update the agent", op)
	}
}

func servicesStatus(ctx context.Context) (agentproto.ServicesStatusResult, *agentproto.OpError) {
	var res agentproto.ServicesStatusResult
	for _, name := range watchedServices {
		out, err := systemctl(ctx, "show", name+".service", "--timestamp=unix",
			"--property=LoadState,ActiveState,SubState,ActiveEnterTimestamp")
		if err != nil {
			return res, systemdError(err)
		}
		res.Services = append(res.Services, parseShow(name, out))
	}
	return res, nil
}

// parseShow reads `systemctl show` key=value output.
func parseShow(name string, out []byte) agentproto.Service {
	s := agentproto.Service{Name: name, Restartable: slices.Contains(restartableServices, name)}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "LoadState":
			s.Installed = v == "loaded"
		case "ActiveState":
			s.ActiveState = v
		case "SubState":
			s.SubState = v
		case "ActiveEnterTimestamp":
			if sec, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64); err == nil && sec > 0 {
				t := time.Unix(sec, 0).UTC()
				s.Since = &t
			}
		}
	}
	if !s.Installed {
		s.Restartable = false
	}
	return s
}

func servicesRestart(ctx context.Context, name string) (map[string]string, *agentproto.OpError) {
	if !slices.Contains(restartableServices, name) {
		return nil, opError(agentproto.ErrNotAllowed, "%q cannot be restarted from the panel", name)
	}
	if _, err := systemctl(ctx, "restart", name+".service"); err != nil {
		return nil, systemdError(err)
	}
	return map[string]string{"restarted": name}, nil
}
