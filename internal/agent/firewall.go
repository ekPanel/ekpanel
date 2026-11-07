package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"ekpanel/internal/agentproto"
)

// firewallStatePath lists the ufw rules ekPanel added, so it can remove the ones it no
// longer wants without touching rules someone added by hand.
const firewallStatePath = "/var/lib/ekpanel-agent/firewall.json"

// basePorts stay open whenever ekPanel runs the firewall: web traffic (HTTP/3 too) and the
// agent port that agents on other servers dial. SSH is added on the ports sshd uses.
var basePorts = []string{"80/tcp", "443/tcp", "443/udp", "7443/tcp"}

func applyFirewall(ctx context.Context, sys *system, spec agentproto.FirewallSpec) *agentproto.FirewallResult {
	res := &agentproto.FirewallResult{SSHPorts: sshPorts(ctx, sys)}
	err := func() error {
		if !spec.Enabled {
			if _, err := sys.run(ctx, "ufw", "status"); err != nil {
				return nil // no ufw, nothing to turn off
			}
			_, err := sys.run(ctx, "ufw", "disable")
			return err
		}
		want, err := firewallRules(spec, res.SSHPorts)
		if err != nil {
			return err
		}
		if err := ensurePackage(ctx, sys, "ufw"); err != nil {
			return fmt.Errorf("install ufw: %w", err)
		}
		var had [][]string
		if b, err := os.ReadFile(sys.path(firewallStatePath)); err == nil {
			json.Unmarshal(b, &had)
		}
		// Each ufw call reloads the firewall, so only the differences are applied. Rules
		// removed by hand are put back; rules added by hand are left alone.
		out, err := sys.run(ctx, "ufw", "show", "added")
		if err != nil {
			return fmt.Errorf("read ufw rules: %w", err)
		}
		present := map[string]bool{}
		for line := range strings.Lines(string(out)) {
			present[strings.TrimSpace(line)] = true
		}
		listed := func(r []string) bool { return present["ufw "+strings.Join(r, " ")+" comment 'ekpanel'"] }
		for _, r := range had {
			if !slices.ContainsFunc(want, func(w []string) bool { return slices.Equal(w, r) }) && listed(r) {
				if _, err := sys.run(ctx, "ufw", append([]string{"delete"}, r...)...); err != nil {
					return fmt.Errorf("ufw delete %s: %w", strings.Join(r, " "), err)
				}
			}
		}
		verbose, _ := sys.run(ctx, "ufw", "status", "verbose")
		if !strings.Contains(string(verbose), "deny (incoming)") || !strings.Contains(string(verbose), "allow (outgoing)") {
			for _, args := range [][]string{{"default", "deny", "incoming"}, {"default", "allow", "outgoing"}} {
				if _, err := sys.run(ctx, "ufw", args...); err != nil {
					return err
				}
			}
		}
		for _, r := range want {
			if listed(r) {
				continue
			}
			// Blocks go first, so they win over any allow below them.
			verb := []string{}
			if r[0] == "deny" {
				verb = []string{"prepend"}
			}
			if _, err := sys.run(ctx, "ufw", append(append(verb, r...), "comment", "ekpanel")...); err != nil {
				return fmt.Errorf("ufw %s: %w", strings.Join(r, " "), err)
			}
		}
		b, _ := json.Marshal(want)
		if err := os.MkdirAll(filepath.Dir(sys.path(firewallStatePath)), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(sys.path(firewallStatePath), b, 0o600); err != nil {
			return err
		}
		if strings.Contains(string(verbose), "Status: active") {
			return nil
		}
		_, err = sys.run(ctx, "ufw", "--force", "enable")
		return err
	}()
	if err != nil {
		res.Error = err.Error()
	}
	if out, err := sys.run(ctx, "ufw", "status"); err == nil {
		res.Active = strings.Contains(string(out), "Status: active")
	}
	return res
}

// sshPorts are the ports sshd listens on, falling back to 22 if they cannot be read, so a
// firewall never goes up without a way back in.
func sshPorts(ctx context.Context, sys *system) []int {
	out, err := sys.run(ctx, "sshd", "-T")
	var ports []int
	if err == nil {
		for line := range strings.Lines(string(out)) {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "port "); ok {
				if p, err := strconv.Atoi(v); err == nil && p > 0 && p < 65536 && !slices.Contains(ports, p) {
					ports = append(ports, p)
				}
			}
		}
	}
	if len(ports) == 0 {
		ports = []int{22}
	}
	slices.Sort(ports)
	return ports
}

// firewallRules turns the spec into ufw rule arguments, base rules first.
func firewallRules(spec agentproto.FirewallSpec, ssh []int) ([][]string, error) {
	var rules [][]string
	for _, p := range ssh {
		rules = append(rules, []string{"allow", strconv.Itoa(p) + "/tcp"})
	}
	for _, p := range basePorts {
		rules = append(rules, []string{"allow", p})
	}
	for _, r := range spec.Rules {
		rs, err := firewallRule(r)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", r.ID, err)
		}
		for _, x := range rs {
			if !slices.ContainsFunc(rules, func(y []string) bool { return slices.Equal(x, y) }) {
				rules = append(rules, x)
			}
		}
	}
	return rules, nil
}

func firewallRule(r agentproto.FirewallRuleSpec) ([][]string, error) {
	source := ""
	if r.Source != "" {
		p, err := netip.ParsePrefix(r.Source)
		if err != nil {
			a, aerr := netip.ParseAddr(r.Source)
			if aerr != nil || a.Zone() != "" {
				return nil, errors.New("invalid address")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		p = p.Masked()
		source = p.String()
		if p.IsSingleIP() {
			source = p.Addr().String()
		}
	}
	port := ""
	switch {
	case r.PortFrom == 0 && r.PortTo == 0:
	case r.PortFrom < 1 || r.PortFrom > 65535 || r.PortTo < r.PortFrom || r.PortTo > 65535:
		return nil, errors.New("invalid port")
	case r.PortTo == r.PortFrom:
		port = strconv.Itoa(r.PortFrom)
	default:
		port = strconv.Itoa(r.PortFrom) + ":" + strconv.Itoa(r.PortTo)
	}
	var protos []string
	switch r.Protocol {
	case "tcp", "udp":
		protos = []string{r.Protocol}
	case "both":
		protos = []string{"tcp", "udp"}
	default:
		return nil, errors.New("invalid protocol")
	}
	var out [][]string
	switch {
	case r.Action == "allow" && port != "" && source == "":
		for _, pr := range protos {
			out = append(out, []string{"allow", port + "/" + pr})
		}
	case (r.Action == "allow" || r.Action == "deny") && port != "" && source != "":
		for _, pr := range protos {
			out = append(out, []string{r.Action, "from", source, "to", "any", "port", port, "proto", pr})
		}
	case r.Action == "deny" && port == "" && source != "":
		out = append(out, []string{"deny", "from", source})
	default:
		return nil, errors.New("a rule needs a port to open or an address to block")
	}
	return out, nil
}
