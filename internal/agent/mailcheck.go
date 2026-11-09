package agent

import (
	"bufio"
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ekpanel/internal/agentproto"
)

// blocklists are asked whether the server's IPv4 addresses send spam. Spamhaus answers
// 127.255.255.x instead of a listing to resolvers it does not serve (large public ones).
var blocklists = []string{"zen.spamhaus.org", "bl.spamcop.net", "b.barracudacentral.org"}

// port25Probe is a mail server that accepts connections from anywhere.
const port25Probe = "gmail-smtp-in.l.google.com:25"

func mailCheck(ctx context.Context, p agentproto.MailCheckParams) (agentproto.MailCheckResult, *agentproto.OpError) {
	if !agentproto.ValidMailHost(p.Hostname) {
		return agentproto.MailCheckResult{}, opError(agentproto.ErrInvalidParams, "invalid host name")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res := agentproto.MailCheckResult{Hostname: p.Hostname, HostnameAddresses: []string{}, Addresses: []agentproto.MailAddressCheck{}}
	if addrs, err := net.DefaultResolver.LookupHost(ctx, p.Hostname); err == nil {
		res.HostnameAddresses = addrs
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.Port25 = probePort25(ctx)
	}()
	// Only addresses other mail servers can reach (as the panel's DNS sync picks them).
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	siteLocal := &net.IPNet{IP: net.ParseIP("fec0::"), Mask: net.CIDRMask(10, 128)}
	for _, a := range globalAddresses() {
		ip := net.ParseIP(a)
		if ip == nil || ip.IsPrivate() || cgnat.Contains(ip) || siteLocal.Contains(ip) {
			continue
		}
		c := agentproto.MailAddressCheck{Address: a, PTR: []string{}, Listed: []string{}, Unknown: []string{}, Checked: []string{}}
		names, _ := net.DefaultResolver.LookupAddr(ctx, a)
		for _, n := range names {
			n = strings.ToLower(strings.TrimSuffix(n, "."))
			c.PTR = append(c.PTR, n)
			c.PTROK = c.PTROK || n == p.Hostname
		}
		if v4 := ip.To4(); v4 != nil {
			rev := []string{}
			for i := 3; i >= 0; i-- {
				rev = append(rev, strconv.Itoa(int(v4[i])))
			}
			for _, bl := range blocklists {
				c.Checked = append(c.Checked, bl)
				switch blocklisted(ctx, strings.Join(rev, ".")+"."+bl) {
				case "listed":
					c.Listed = append(c.Listed, bl)
				case "unknown":
					c.Unknown = append(c.Unknown, bl)
				}
			}
		}
		res.Addresses = append(res.Addresses, c)
	}
	wg.Wait()
	return res, nil
}

// blocklisted is "listed", "clean" or "unknown" for one DNS blocklist query.
func blocklisted(ctx context.Context, name string) string {
	addrs, err := net.DefaultResolver.LookupHost(ctx, name)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return "clean"
		}
		return "unknown"
	}
	if slices.ContainsFunc(addrs, func(a string) bool { return strings.HasPrefix(a, "127.255.255.") }) {
		return "unknown"
	}
	if slices.ContainsFunc(addrs, func(a string) bool { return strings.HasPrefix(a, "127.") }) {
		return "listed"
	}
	return "unknown"
}

// probePort25 is "open" if a mail server outside answers on port 25, else "blocked".
func probePort25(ctx context.Context) string {
	d := net.Dialer{Timeout: 8 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", port25Probe)
	if err != nil {
		return "blocked"
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "220") {
		return "blocked"
	}
	return "open"
}
