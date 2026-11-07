package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// A Git app is released without downtime: a container of the new build starts next to the
// running one (on the other of the app's two host ports), and once it answers, the old one
// stops. The web server tries both ports, so requests always reach a running container. If
// the new container does not come up, it is removed and the old one keeps serving.

const releaseTimeout = 3 * time.Minute

// waitReady waits until something answers on a host port. Tests replace it.
var waitReady = waitPort

// gitContainers lists the running containers of a Git app.
func gitContainers(ctx context.Context, sys *system, name string) ([]string, error) {
	out, err := sys.run(ctx, "docker", "ps", "--quiet", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+appProject(name),
		"--filter", "label=com.docker.compose.service="+agentproto.AppServiceName)
	return strings.Fields(string(out)), err
}

func inspect(ctx context.Context, sys *system, format, id string) string {
	out, _ := sys.run(ctx, "docker", "inspect", "--format", format, id)
	return strings.TrimSpace(string(out))
}

// releaseGitApp brings the app to its build and settings, replacing a running container
// only once its successor is ready.
func releaseGitApp(ctx context.Context, sys *system, ap agentproto.AppSpec) error {
	running, err := gitContainers(ctx, sys, ap.Name)
	if err != nil {
		return err
	}
	up := func(args ...string) error {
		out, err := compose(ctx, sys, ap.Name, ap.Env, append([]string{"up", "--detach"}, args...)...)
		if err != nil {
			logs, _ := compose(ctx, sys, ap.Name, ap.Env, "logs", "--no-color", "--tail", "5")
			return fmt.Errorf("%w %s", err, lastLines(append(out, logs...), 5))
		}
		return nil
	}
	if len(running) == 0 {
		return up("--remove-orphans", "--wait", "--wait-timeout", "120")
	}
	if len(running) == 1 {
		// Compose labels each container with a hash of its settings (image, variables,
		// limits…); the same hash means the running container is already what is wanted.
		out, err := compose(ctx, sys, ap.Name, ap.Env, "config", "--hash", agentproto.AppServiceName)
		fields := strings.Fields(string(out))
		if err == nil && len(fields) == 2 && inspect(ctx, sys, `{{index .Config.Labels "com.docker.compose.config-hash"}}`, running[0]) == fields[1] {
			return up("--no-recreate") // nothing new; just make sure it runs
		}
	}

	// One more container of the service, from the current file; --no-recreate keeps the old.
	if err := up("--no-deps", "--no-recreate", "--scale", agentproto.AppServiceName+"="+strconv.Itoa(len(running)+1), agentproto.AppServiceName); err != nil {
		return fmt.Errorf("the new release could not start: %w", err)
	}
	now, err := gitContainers(ctx, sys, ap.Name)
	if err != nil {
		return err
	}
	var fresh []string
	for _, id := range now {
		if !slices.Contains(running, id) {
			fresh = append(fresh, id)
		}
	}
	if len(fresh) != 1 {
		return fmt.Errorf("expected one new container, found %d", len(fresh))
	}
	next := fresh[0]
	if err := waitForContainer(ctx, sys, next, ap.ContainerPort); err != nil {
		logs, _ := sys.run(ctx, "docker", "logs", "--tail", "10", next)
		sys.run(context.WithoutCancel(ctx), "docker", "rm", "--force", next)
		return fmt.Errorf("the new release did not start, so the previous one keeps running: %v %s", err, lastLines(logs, 5))
	}
	for _, old := range running {
		sys.run(ctx, "docker", "stop", "--time", "30", old)
		sys.run(ctx, "docker", "rm", "--force", old)
	}
	return nil
}

// gitPorts lists a Git app's two host ports, the running container's first, so the web
// server never tries the idle one while a release is not under way.
func gitPorts(ctx context.Context, sys *system, ap agentproto.AppSpec) []int {
	ports := []int{ap.HostPort, ap.HostPort + 1}
	running, _ := gitContainers(ctx, sys, ap.Name)
	if len(running) != 1 {
		return ports
	}
	out, _ := sys.run(ctx, "docker", "port", running[0], strconv.Itoa(ap.ContainerPort)+"/tcp")
	_, port, _ := strings.Cut(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]), ":")
	if port == strconv.Itoa(ap.HostPort+1) {
		ports[0], ports[1] = ports[1], ports[0]
	}
	return ports
}

// waitForContainer waits for a new container to be ready: healthy, if its image has a
// health check; otherwise answering on its published port.
func waitForContainer(ctx context.Context, sys *system, id string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	addr := ""
	for {
		state := strings.Fields(inspect(ctx, sys, "{{.State.Status}} {{.RestartCount}} {{if .State.Health}}{{.State.Health.Status}}{{end}}", id))
		status, restarts, health := "", "", ""
		if len(state) >= 2 {
			status, restarts = state[0], state[1]
		}
		if len(state) == 3 {
			health = state[2]
		}
		switch {
		case status == "exited" || status == "dead":
			return errors.New("the container stopped")
		case status == "restarting" || (restarts != "" && restarts != "0"):
			return errors.New("the container keeps stopping and restarting")
		case health == "unhealthy":
			return errors.New("the container's health check failed")
		case health == "healthy":
			return nil
		case health == "" && status == "running":
			if addr == "" {
				out, _ := sys.run(ctx, "docker", "port", id, strconv.Itoa(port)+"/tcp")
				addr = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
			}
			if addr != "" {
				return waitReady(ctx, addr)
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("it was not ready within 3 minutes")
		case <-time.After(time.Second):
		}
	}
}

// waitPort waits until the app answers on addr: an HTTP response of any kind, or for apps
// that do not speak HTTP, a connection that stays open.
func waitPort(ctx context.Context, addr string) error {
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	accepted := 0
	for {
		if resp, err := client.Get("http://" + addr + "/"); err == nil {
			resp.Body.Close()
			return nil
		}
		if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
			// Docker's proxy accepts connections even before the app listens; a real
			// server keeps it open instead of closing it straight away.
			c.SetReadDeadline(time.Now().Add(time.Second))
			_, rerr := bufio.NewReader(c).ReadByte()
			c.Close()
			var ne net.Error
			if errors.As(rerr, &ne) && ne.Timeout() {
				if accepted++; accepted >= 3 {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return errors.New("it did not answer within 3 minutes")
		case <-time.After(2 * time.Second):
		}
	}
}

// pruneBuilds removes the app's builds that are neither running nor kept for rollbacks.
func pruneBuilds(ctx context.Context, sys *system, ap agentproto.AppSpec) {
	out, err := sys.run(ctx, "docker", "images", "--format", "{{.Repository}}:{{.Tag}}", agentproto.GitImageRepo(ap.Name))
	if err != nil {
		return
	}
	keep := append([]string{ap.Image}, ap.KeepImages...)
	for _, img := range strings.Fields(string(out)) {
		if gitImageOK(ap.Name, img) && !slices.Contains(keep, img) {
			sys.run(ctx, "docker", "rmi", img)
		}
	}
}
