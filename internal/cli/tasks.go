package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type taskInfo struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	ServerName *string    `json:"server_name"`
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	Error      *string    `json:"error"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at"`
}

func (t taskInfo) finished() bool { return t.Status != "queued" && t.Status != "running" }

// maxTaskList is the most tasks GET /tasks returns.
const maxTaskList = 200

// waitTask waits for a task to finish and returns its last JSON. A failed task is an error.
func (a *app) waitTask(c *client, id string, timeout time.Duration) ([]byte, error) {
	var raw []byte
	last := ""
	var t taskInfo
	err := a.waitFor(timeout, func(ctx context.Context) (bool, error) {
		var r json.RawMessage
		if _, err := c.get(ctx, "tasks/"+url.PathEscape(id), &r); err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				return false, fmt.Errorf("No task %s. See 'ekctl tasks list'.", id)
			}
			return false, err
		}
		if err := json.Unmarshal(r, &t); err != nil {
			return false, err
		}
		raw = r
		if t.Status != last {
			last = t.Status
			a.progress("Task %s (%s): %s", t.ID, t.Kind, t.Status)
		}
		return t.finished(), nil
	})
	if err != nil {
		return raw, err
	}
	if t.Status != "succeeded" {
		return raw, &taskFailedError{t}
	}
	return raw, nil
}

type taskFailedError struct{ t taskInfo }

func (e *taskFailedError) Error() string {
	msg := fmt.Sprintf("Task %s (%s) %s", e.t.ID, e.t.Kind, e.t.Status)
	if e.t.Error != nil {
		msg += ": " + *e.t.Error
	}
	return msg
}

// finishTask prints a task after waiting for it.
func (a *app) finishTask(raw []byte, err error) error {
	var tf *taskFailedError
	if a.g.JSON && (err == nil || errors.As(err, &tf)) {
		if perr := a.printJSON(raw); perr != nil {
			return perr
		}
	}
	if err != nil {
		return err
	}
	if !a.g.JSON {
		var t taskInfo
		json.Unmarshal(raw, &t)
		fmt.Fprintf(a.env.Stdout, "Task %s (%s) succeeded.\n", t.ID, t.Kind)
	}
	return nil
}

func (a *app) tasksCmd() *command {
	return &command{
		name:    "tasks",
		summary: "Follow the panel's background work",
		long: "Follow the panel's background work: setting up servers, backups, deployments and more.\n\n" +
			"Commands that start a task print its ID; 'ekctl tasks wait ID' waits for it.",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List recent tasks",
				run: func(fs *flag.FlagSet) func([]string) error {
					active := fs.Bool("active", false, "only tasks that are queued or running")
					server := fs.String("server", "", "only the tasks of this `server` (ID or name)")
					limit := fs.Int("limit", 20, "show at most this many tasks (`n`, up to 200)")
					return func([]string) error {
						if *limit < 1 || *limit > maxTaskList {
							return usagef("--limit is from 1 to %d.", maxTaskList)
						}
						c, err := a.client()
						if err != nil {
							return err
						}
						q := url.Values{"limit": {strconv.Itoa(*limit)}}
						if *active {
							q.Set("active", "1")
						}
						if *server != "" {
							id, err := a.resolve(c, "server", "servers", *server)
							if err != nil {
								return err
							}
							q.Set("server_id", id)
						}
						var list []taskInfo
						raw, err := c.get(a.ctx, "tasks?"+q.Encode(), &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, t := range list {
							rows = append(rows, []string{t.ID, t.Kind, t.Status, str(t.ServerName), fmtTime(t.CreatedAt), truncate(str(t.Error), 50)})
						}
						return a.table([]string{"ID", "KIND", "STATUS", "SERVER", "CREATED", "ERROR"}, rows, "No tasks.")
					}
				},
			},
			{
				name: "wait", summary: "Wait for a task to finish", args: []string{"TASK_ID"},
				long:     "Wait for a task to finish. The exit code is 0 when it succeeded and 1 when it failed.",
				examples: []string{"ekctl tasks wait 6f1d…-…-… --timeout 30m"},
				run: func(fs *flag.FlagSet) func([]string) error {
					timeout := fs.Duration("timeout", defaultWaitTimeout, "stop waiting after this `duration`")
					return func(args []string) error {
						if !isUUID(args[0]) {
							return usagef("%q is not a task ID. Task IDs look like 6f1d2c3e-…; see 'ekctl tasks list'.", args[0])
						}
						c, err := a.client()
						if err != nil {
							return err
						}
						return a.finishTask(a.waitTask(c, strings.ToLower(args[0]), *timeout))
					}
				},
			},
		},
	}
}

// ---- Backups ----

func (a *app) backupsCmd() *command {
	return &command{
		name:    "backups",
		summary: "List backup repositories and run backups",
		subs: []*command{
			{
				name: "list", aliases: []string{"ls"}, summary: "List backup repositories",
				run: func(fs *flag.FlagSet) func([]string) error {
					return func([]string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						var list []struct {
							ID         string     `json:"id"`
							ServerName string     `json:"server_name"`
							Name       string     `json:"name"`
							Kind       string     `json:"kind"`
							Schedule   string     `json:"schedule"`
							LastRunAt  *time.Time `json:"last_run_at"`
							LastStatus *string    `json:"last_status"`
							NextRunAt  *time.Time `json:"next_run_at"`
						}
						raw, err := c.get(a.ctx, "backups", &list)
						if err != nil || a.g.JSON {
							return a.orJSON(raw, err)
						}
						rows := [][]string{}
						for _, b := range list {
							rows = append(rows, []string{b.ID, b.Name, b.Kind, b.ServerName, b.Schedule, fmtTimePtr(b.LastRunAt), str(b.LastStatus), fmtTimePtr(b.NextRunAt)})
						}
						return a.table([]string{"ID", "NAME", "KIND", "SERVER", "SCHEDULE", "LAST RUN", "LAST STATUS", "NEXT RUN"}, rows, "No backup repositories.")
					}
				},
			},
			{
				name: "run", summary: "Back up a repository's server now", args: []string{"REPO"},
				long:     "Back up now to a backup repository. REPO is its ID or name.",
				examples: []string{"ekctl backups run offsite --wait"},
				run: func(fs *flag.FlagSet) func([]string) error {
					wait, timeout := waitFlags(fs, "the backup has finished")
					return func(args []string) error {
						c, err := a.client()
						if err != nil {
							return err
						}
						id, err := a.resolve(c, "backup repository", "backups", args[0])
						if err != nil {
							return err
						}
						var t taskInfo
						raw, err := c.call(a.ctx, http.MethodPost, "backups/"+id+"/run", nil, &t)
						if err != nil {
							return err
						}
						if !*wait {
							if a.g.JSON {
								return a.printJSON(raw)
							}
							fmt.Fprintf(a.env.Stdout, "Started the backup. Task %s.\n", t.ID)
							fmt.Fprintf(a.env.Stderr, "Wait for it with: ekctl tasks wait %s\n", t.ID)
							return nil
						}
						return a.finishTask(a.waitTask(c, t.ID, *timeout))
					}
				},
			},
		},
	}
}

// ---- api ----

var apiMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE"}

func (a *app) apiCmd() *command {
	return &command{
		name:    "api",
		summary: "Call any API endpoint",
		args:    []string{"METHOD", "PATH"},
		usage:   "METHOD PATH [--data JSON|@FILE|-]",
		long: "Call any API endpoint and print its answer.\n\n" +
			"PATH is relative to /api/v1 (sites/ID) or absolute (/api/v1/sites/ID). The endpoints are listed\n" +
			"in the panel's API description: ekctl api GET openapi.json. The exit code is 1 when the panel\n" +
			"answers with an error; the error's JSON is still printed.",
		examples: []string{
			"ekctl api GET sites",
			"ekctl api PATCH sites/0b6c… --data '{\"kind\":\"php\",\"php_version\":\"8.4\"}'",
			"ekctl api POST cron --data @job.json",
			"echo '{\"name\":\"web2\"}' | ekctl api POST servers --data -",
		},
		run: func(fs *flag.FlagSet) func([]string) error {
			data := fs.String("data", "", "the request body: `JSON`, @FILE to read a file, or - to read stdin")
			return func(args []string) error {
				method := strings.ToUpper(args[0])
				if !slices.Contains(apiMethods, method) {
					return usagef("%q is not a method. Use one of %s.", args[0], strings.Join(apiMethods, ", "))
				}
				path := args[1]
				if strings.Contains(path, "://") {
					return usagef("Give the path only, such as sites or /api/v1/sites. The panel URL comes from login or --url.")
				}
				var body []byte
				switch {
				case *data == "":
				case *data == "-":
					b, err := io.ReadAll(a.in)
					if err != nil {
						return fmt.Errorf("Cannot read the body from stdin: %v", err)
					}
					body = b
				case strings.HasPrefix(*data, "@"):
					b, err := os.ReadFile((*data)[1:])
					if err != nil {
						return usagef("Cannot read the body file: %v", err)
					}
					body = b
				default:
					body = []byte(*data)
				}
				if body != nil && !json.Valid(body) {
					return usagef("The body is not valid JSON.")
				}
				c, err := a.client()
				if err != nil {
					return err
				}
				raw, err := c.do(a.ctx, method, path, body)
				if perr := a.printJSON(raw); perr != nil && err == nil {
					return perr
				}
				return err
			}
		},
	}
}
