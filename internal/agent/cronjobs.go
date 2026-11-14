package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"ekpanel/internal/agentproto"
	"ekpanel/internal/cron"
)

// Each job is a systemd timer and a oneshot service. A job that is still running when its
// next time comes is not started twice, it stops after its timeout, and its output goes to
// the journal. The command itself sits in a script file, so no quoting or "%" in it can
// change the unit.
const cronScriptDir = "/etc/ekpanel/cron"

func cronUnit(id string) string   { return "ekpanel-cron-" + id }
func cronScript(id string) string { return cronScriptDir + "/" + id + ".sh" }

func validateCron(c agentproto.CronSpec) error {
	if !uuidRE.MatchString(c.ID) {
		return errors.New("invalid job id")
	}
	if c.Owner != "" && !uuidRE.MatchString(c.Owner) {
		return errors.New("invalid owner")
	}
	if c.Deleted {
		return nil
	}
	switch {
	case c.User != "" && c.App == "":
		if !userRE.MatchString(c.User) || slices.Contains(reserved, c.User) {
			return fmt.Errorf("invalid account %q", c.User)
		}
	case c.App != "" && c.User == "":
		if !appNameRE.MatchString(c.App) || !serviceRE.MatchString(c.Service) {
			return errors.New("invalid app or service")
		}
	default:
		return errors.New("a job runs for a website or an app")
	}
	if _, err := cron.Parse(c.Schedule); err != nil {
		return err
	}
	if !cron.ValidZone(c.Timezone) {
		return fmt.Errorf("unknown time zone %q", c.Timezone)
	}
	if strings.TrimSpace(c.Command) == "" || len(c.Command) > 4096 || strings.ContainsRune(c.Command, 0) {
		return errors.New("the command is empty or too long")
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 86400 {
		return errors.New("invalid timeout")
	}
	return nil
}

// shellQuote puts s in single quotes for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func cronScriptBody(c agentproto.CronSpec) string {
	if c.User != "" {
		return "#!/bin/sh\n# Managed by ekPanel (cron job " + c.ID + "). Changes here are overwritten.\n" +
			"cd /home/" + c.User + "/public_html 2>/dev/null || cd /home/" + c.User + "\n" + c.Command + "\n"
	}
	project, service := appProject(c.App), c.Service
	return "#!/bin/sh\n# Managed by ekPanel (cron job " + c.ID + "). Changes here are overwritten.\n" +
		"c=$(docker ps --quiet --filter label=com.docker.compose.project=" + project +
		" --filter label=com.docker.compose.service=" + service + " | head -n 1)\n" +
		"[ -n \"$c\" ] || { echo \"" + service + " of app " + c.App + " is not running\" >&2; exit 1; }\n" +
		"exec docker exec \"$c\" sh -c " + shellQuote(c.Command) + "\n"
}

var cronServiceTemplate = template.Must(template.New("service").Parse(`# Managed by ekPanel. Changes here are overwritten.
[Unit]
Description=ekPanel cron job {{.ID}}

[Service]
Type=oneshot
{{- if .User}}
User={{.User}}
Group={{.User}}
WorkingDirectory=/home/{{.User}}
ProtectSystem=full
PrivateTmp=yes
NoNewPrivileges=yes
{{- end}}
{{- if .Slice}}
Slice={{.Slice}}
{{- end}}
ExecStart=/bin/sh {{.Script}}
TimeoutStartSec={{.Timeout}}
Nice=5
`))

var cronTimerTemplate = template.Must(template.New("timer").Parse(`# Managed by ekPanel. Changes here are overwritten.
[Unit]
Description=ekPanel cron job {{.ID}}

[Timer]
{{- range .Calendar}}
OnCalendar={{.}}
{{- end}}
AccuracySec=1s
Unit={{.Unit}}.service

[Install]
WantedBy=timers.target
`))

// applyCron converges the server's timers to the jobs given and returns a result per job.
// Timers of jobs the panel no longer knows are removed.
func (a *applier) applyCron(ctx context.Context, jobs []agentproto.CronSpec) []agentproto.AppResult {
	results := make([]agentproto.AppResult, 0, len(jobs))
	known := map[string]bool{}
	reload := false
	type pending struct {
		unit    string
		enable  bool
		restart bool
		idx     int
	}
	var actions []pending
	fail := func(i int, err error) { results[i].OK, results[i].Error = false, err.Error() }

	for i, c := range jobs {
		results = append(results, agentproto.AppResult{ID: c.ID, Generation: c.Generation, OK: true})
		known[c.ID] = true
		if err := validateCron(c); err != nil {
			fail(i, err)
			continue
		}
		if c.Deleted {
			if a.removeCron(ctx, c.ID) {
				reload = true
			}
			continue
		}
		if c.User != "" {
			if exists, err := a.managedUser(ctx, c.User); err != nil || !exists {
				fail(i, fmt.Errorf("the website's account %s is not ready", c.User))
				continue
			}
		}
		s, _ := cron.Parse(c.Schedule)
		calendar := s.OnCalendar(c.Timezone)
		for _, v := range calendar {
			if _, err := a.sys.run(ctx, "systemd-analyze", "calendar", v); err != nil {
				fail(i, fmt.Errorf("systemd cannot use the schedule: %w", err))
				break
			}
		}
		if !results[i].OK {
			continue
		}
		changed, err := a.writeCronFiles(ctx, c, calendar)
		if err != nil {
			fail(i, err)
			continue
		}
		reload = reload || changed
		actions = append(actions, pending{unit: cronUnit(c.ID), enable: c.Enabled, restart: changed, idx: i})
	}
	// Jobs removed from the panel without a delete (their site or app is gone).
	units, _ := filepath.Glob(a.sys.path(systemdDir + "/ekpanel-cron-*.timer"))
	for _, p := range units {
		id := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(p), "ekpanel-cron-"), ".timer")
		if uuidRE.MatchString(id) && !known[id] && a.removeCron(ctx, id) {
			reload = true
		}
	}
	if reload {
		if _, err := a.sys.run(ctx, "systemctl", "daemon-reload"); err != nil {
			for _, p := range actions {
				fail(p.idx, err)
			}
			return results
		}
	}
	for _, p := range actions {
		timer := p.unit + ".timer"
		active, _ := a.sys.run(ctx, "systemctl", "is-active", timer)
		running := strings.TrimSpace(string(active)) == "active"
		var err error
		switch {
		case p.enable && !running:
			_, err = a.sys.run(ctx, "systemctl", "enable", "--now", timer)
		case p.enable && p.restart:
			// A running timer keeps its old schedule until restarted.
			_, err = a.sys.run(ctx, "systemctl", "restart", timer)
		case !p.enable && running:
			_, err = a.sys.run(ctx, "systemctl", "disable", "--now", timer)
		}
		if err != nil {
			fail(p.idx, err)
		}
	}
	return results
}

// writeCronFiles writes a job's script, service and timer, reporting whether any changed.
func (a *applier) writeCronFiles(ctx context.Context, c agentproto.CronSpec, calendar []string) (bool, error) {
	if err := os.MkdirAll(a.sys.path(cronScriptDir), 0o711); err != nil {
		return false, err
	}
	// The script holds the command, which may contain secrets: only root and, for a
	// website's job, that site's account can read it.
	script := a.sys.path(cronScript(c.ID))
	scriptChanged, err := writeSecretIfChanged(script, []byte(cronScriptBody(c)))
	if err != nil {
		return false, err
	}
	if c.User != "" {
		if _, err := a.sys.run(ctx, "chown", "root:"+c.User, cronScript(c.ID)); err != nil {
			return false, err
		}
		if err := os.Chmod(script, 0o640); err != nil {
			return false, err
		}
	}
	vars := map[string]any{"ID": c.ID, "User": c.User, "Script": cronScript(c.ID), "Timeout": strconv.Itoa(c.TimeoutSeconds),
		"Calendar": calendar, "Unit": cronUnit(c.ID)}
	if c.Owner != "" {
		vars["Slice"] = customerSlice(ownerKey(c.Owner))
	}
	var service, timer bytes.Buffer
	cronServiceTemplate.Execute(&service, vars)
	cronTimerTemplate.Execute(&timer, vars)
	serviceChanged, _, err := a.writeIfChanged(systemdDir+"/"+cronUnit(c.ID)+".service", service.Bytes())
	if err != nil {
		return false, err
	}
	timerChanged, _, err := a.writeIfChanged(systemdDir+"/"+cronUnit(c.ID)+".timer", timer.Bytes())
	if err != nil {
		return false, err
	}
	return scriptChanged || serviceChanged || timerChanged, nil
}

// removeCron stops a job's timer and removes its files. It reports whether units changed.
func (a *applier) removeCron(ctx context.Context, id string) bool {
	timer := a.sys.path(systemdDir + "/" + cronUnit(id) + ".timer")
	if _, err := os.Stat(timer); err != nil {
		os.Remove(a.sys.path(cronScript(id)))
		return false
	}
	a.sys.run(ctx, "systemctl", "disable", "--now", cronUnit(id)+".timer")
	a.sys.run(ctx, "systemctl", "stop", cronUnit(id)+".service")
	os.Remove(timer)
	os.Remove(a.sys.path(systemdDir + "/" + cronUnit(id) + ".service"))
	os.Remove(a.sys.path(cronScript(id)))
	return true
}

// ---- Operations ----

func cronExists(sys *system, id string) *agentproto.OpError {
	if !uuidRE.MatchString(id) {
		return opError(agentproto.ErrInvalidParams, "invalid job id")
	}
	if _, err := os.Stat(sys.path(systemdDir + "/" + cronUnit(id) + ".service")); err != nil {
		return opError(agentproto.ErrNotAllowed, "no such job on this server")
	}
	return nil
}

func cronRun(ctx context.Context, sys *system, p agentproto.CronParams) (any, *agentproto.OpError) {
	if opErr := cronExists(sys, p.ID); opErr != nil {
		return nil, opErr
	}
	if _, err := sys.run(ctx, "systemctl", "start", "--no-block", cronUnit(p.ID)+".service"); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return struct{}{}, nil
}

// showValues reads "Key=Value" lines from systemctl show.
func showValues(out []byte) map[string]string {
	m := map[string]string{}
	for line := range strings.Lines(string(out)) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			m[k] = v
		}
	}
	return m
}

func cronStatus(ctx context.Context, sys *system, p agentproto.CronParams) (*agentproto.CronStatusResult, *agentproto.OpError) {
	if opErr := cronExists(sys, p.ID); opErr != nil {
		return nil, opErr
	}
	unit := cronUnit(p.ID)
	out, err := sys.run(ctx, "systemctl", "show", unit+".service", "--timestamp=unix",
		"-p", "ActiveState", "-p", "Result", "-p", "ExecMainStartTimestamp", "-p", "ExecMainExitTimestamp", "-p", "ExecMainStatus")
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	svc := showValues(out)
	res := &agentproto.CronStatusResult{Running: svc["ActiveState"] == "activating", Result: svc["Result"]}
	unix := func(v string) *time.Time {
		n, err := strconv.ParseInt(strings.TrimPrefix(v, "@"), 10, 64)
		if err != nil || n <= 0 {
			return nil
		}
		t := time.Unix(n, 0).UTC()
		return &t
	}
	res.LastRun, res.LastEnd = unix(svc["ExecMainStartTimestamp"]), unix(svc["ExecMainExitTimestamp"])
	if res.LastRun != nil && !res.Running {
		if code, err := strconv.Atoi(svc["ExecMainStatus"]); err == nil {
			res.ExitCode = &code
		}
	}
	// systemd prints this one as text whatever --timestamp says; read it in UTC.
	if out, err := sys.exec(ctx, command{name: "systemctl", args: []string{"show", unit + ".timer", "-p", "NextElapseUSecRealtime"}, env: []string{"TZ=UTC"}}); err == nil {
		if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", showValues(out)["NextElapseUSecRealtime"]); err == nil {
			t = t.UTC()
			res.NextRun = &t
		}
	}
	if out, err := sys.run(ctx, "journalctl", "--unit", unit+".service", "--lines", "200", "--no-pager", "--output", "short-iso"); err == nil {
		res.Output = strings.ToValidUTF8(string(out), "�")
	}
	return res, nil
}

// cronResults reads the last finished run of each job in one systemctl call. Unknown ids,
// jobs never run and jobs still running are left out.
func cronResults(ctx context.Context, sys *system, p agentproto.CronResultsParams) (any, *agentproto.OpError) {
	if len(p.IDs) > 5000 {
		return nil, opError(agentproto.ErrInvalidParams, "too many jobs")
	}
	args := []string{"show", "--timestamp=unix", "-p", "Id", "-p", "ActiveState", "-p", "Result",
		"-p", "ExecMainStartTimestamp", "-p", "ExecMainExitTimestamp", "-p", "ExecMainStatus"}
	byUnit := map[string]string{}
	for _, id := range p.IDs {
		if !uuidRE.MatchString(id) {
			return nil, opError(agentproto.ErrInvalidParams, "invalid job id")
		}
		if _, err := os.Stat(sys.path(systemdDir + "/" + cronUnit(id) + ".service")); err != nil {
			continue
		}
		byUnit[cronUnit(id)+".service"] = id
		args = append(args, cronUnit(id)+".service")
	}
	res := agentproto.CronResultsResult{Jobs: []agentproto.CronResult{}}
	if len(byUnit) == 0 {
		return res, nil
	}
	out, err := sys.run(ctx, "systemctl", args...)
	if err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	for block := range strings.SplitSeq(string(out), "\n\n") {
		v := showValues([]byte(block))
		id, ok := byUnit[v["Id"]]
		if !ok || v["ActiveState"] == "activating" {
			continue
		}
		start, err1 := strconv.ParseInt(strings.TrimPrefix(v["ExecMainStartTimestamp"], "@"), 10, 64)
		end, err2 := strconv.ParseInt(strings.TrimPrefix(v["ExecMainExitTimestamp"], "@"), 10, 64)
		if err1 != nil || err2 != nil || start <= 0 || end <= 0 {
			continue
		}
		code, _ := strconv.Atoi(v["ExecMainStatus"])
		res.Jobs = append(res.Jobs, agentproto.CronResult{ID: id, Started: time.Unix(start, 0).UTC(), Ended: time.Unix(end, 0).UTC(),
			Result: v["Result"], ExitCode: code})
	}
	return res, nil
}
