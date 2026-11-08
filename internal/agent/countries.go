package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ekpanel/internal/agentproto"
)

// Country blocking is an nftables table of its own, hooked before everything else that
// filters incoming traffic (ufw and CrowdSec's bouncer included, and Docker's forwarded
// ports), that drops new connections by the sender's country.

const (
	geoDir         = "/var/lib/ekpanel/geo"
	geoList        = geoDir + "/dbip-country-lite.csv.gz"
	geoMonth       = geoDir + "/month"   // the month of geoList, such as "2026-11"
	geoChecked     = geoDir + "/checked" // touched on every download attempt
	countriesNFT   = "/etc/ekpanel/countries.nft"
	countriesUnit  = "/etc/systemd/system/ekpanel-countries.service"
	countriesTable = "ekpanel_countries"
	// geoMinRows is far fewer rows than the list has (over 700,000); a shorter file is
	// not the list.
	geoMinRows = 100_000
	// countriesFormat changes with the rules' layout, so agents replace older files.
	countriesFormat = 2
)

// geoURL is DB-IP's free country list of a month.
func geoURL(month string) string {
	return "https://download.db-ip.com/free/dbip-country-lite-" + month + ".csv.gz"
}

// neverBlocked are networks no country rule applies to: loopback, private, link-local and
// multicast addresses.
var neverBlocked = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.168.0.0/16", "224.0.0.0/4", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8"}

const countriesService = `# Managed by ekPanel.
[Unit]
Description=ekPanel country blocking
After=network-pre.target
Wants=network-pre.target
Before=network.target
ConditionPathExists=` + countriesNFT + `

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/sbin/nft -f ` + countriesNFT + `

[Install]
WantedBy=multi-user.target
`

// applyCountries makes the firewall block what spec asks; exempt is the allowlist.
func applyCountries(ctx context.Context, sys *system, spec agentproto.CountryBlock, exempt []string) agentproto.CountriesStatus {
	if spec.Mode == "" || spec.Mode == agentproto.CountriesOff {
		removeCountries(ctx, sys)
		return agentproto.CountriesStatus{}
	}
	st, err := func() (agentproto.CountriesStatus, error) {
		var st agentproto.CountriesStatus
		if err := spec.Validate(); err != nil {
			return st, err
		}
		month, err := ensureGeoList(ctx, sys, time.Now())
		if err != nil {
			return st, err
		}
		st.List = month
		if err := ensurePackage(ctx, sys, "nftables"); err != nil {
			return st, fmt.Errorf("install nftables: %w", err)
		}
		exempt = append(append(append(slices.Clone(exempt), neverBlocked...), agentproto.CloudflareRanges...), globalAddresses()...)
		codes := slices.Clone(spec.Countries)
		sort.Strings(codes)
		sort.Strings(exempt)
		info, _ := os.Stat(sys.path(geoList))
		var size int64
		if info != nil {
			size = info.Size()
		}
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d|%s|%s|%s", countriesFormat, month, size, spec.Mode, strings.Join(codes, ","), strings.Join(exempt, ","))))
		key := hex.EncodeToString(sum[:16])
		if old, err := os.ReadFile(sys.path(countriesNFT)); err == nil && nftKey(old) == key && countriesLoaded(ctx, sys) {
			st.Active, st.Ranges = true, nftRanges(old)
			return st, nil
		}
		v4, v6, err := countryRanges(sys.path(geoList), codes)
		if err != nil {
			return st, err
		}
		conf := countriesConfig(key, spec.Mode, v4, v6, exempt)
		tmp := countriesNFT + ".new"
		if err := os.MkdirAll(sys.path("/etc/ekpanel"), 0o755); err != nil {
			return st, err
		}
		if err := os.WriteFile(sys.path(tmp), conf, 0o644); err != nil {
			return st, err
		}
		if _, err := sys.run(ctx, "nft", "-f", tmp); err != nil {
			os.Remove(sys.path(tmp))
			return st, fmt.Errorf("load the country rules: %w", err)
		}
		if err := os.Rename(sys.path(tmp), sys.path(countriesNFT)); err != nil {
			return st, err
		}
		a := &applier{sys: sys}
		if changed, _, err := a.writeIfChanged(countriesUnit, []byte(countriesService)); err != nil {
			return st, err
		} else if changed {
			sys.run(ctx, "systemctl", "daemon-reload")
		}
		sys.run(ctx, "systemctl", "enable", "ekpanel-countries")
		st.Active, st.Ranges = true, len(v4)+len(v6)
		return st, nil
	}()
	if err != nil {
		st.Error = err.Error()
		// Rules loaded before keep working; say so rather than claim nothing is blocked.
		st.Active = countriesLoaded(ctx, sys)
	}
	return st
}

func removeCountries(ctx context.Context, sys *system) {
	if countriesLoaded(ctx, sys) {
		sys.run(ctx, "nft", "delete", "table", "inet", countriesTable)
	}
	if _, err := os.Stat(sys.path(countriesUnit)); err == nil {
		sys.run(ctx, "systemctl", "disable", "ekpanel-countries")
		os.Remove(sys.path(countriesUnit))
		sys.run(ctx, "systemctl", "daemon-reload")
	}
	os.Remove(sys.path(countriesNFT))
}

func countriesLoaded(ctx context.Context, sys *system) bool {
	out, err := sys.run(ctx, "nft", "list", "tables")
	return err == nil && slices.Contains(strings.Split(strings.TrimSpace(string(out)), "\n"), "table inet "+countriesTable)
}

// nftKey and nftRanges read the header lines countriesConfig writes.
func nftKey(conf []byte) string { return nftHeader(conf, "# key: ") }

func nftRanges(conf []byte) int {
	n, _ := strconv.Atoi(nftHeader(conf, "# ranges: "))
	return n
}

func nftHeader(conf []byte, prefix string) string {
	for line := range strings.Lines(string(conf)) {
		if v, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(v)
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
	}
	return ""
}

// ensureGeoList keeps the country list of the current month, trying at most once a day
// (DB-IP publishes each month's list during its first days), and returns its month.
func ensureGeoList(ctx context.Context, sys *system, now time.Time) (string, error) {
	have, _ := os.ReadFile(sys.path(geoMonth))
	month := strings.TrimSpace(string(have))
	want := now.UTC().Format("2006-01")
	if month == want {
		return month, nil
	}
	if info, err := os.Stat(sys.path(geoChecked)); err == nil {
		if age := now.Sub(info.ModTime()); month != "" && age < 24*time.Hour {
			return month, nil
		} else if month == "" && age < 10*time.Minute {
			return "", errors.New("the country list could not be downloaded; trying again in a few minutes")
		}
	}
	if err := os.MkdirAll(sys.path(geoDir), 0o755); err != nil {
		return "", err
	}
	os.WriteFile(sys.path(geoChecked), nil, 0o644)
	os.Chtimes(sys.path(geoChecked), now, now)
	var lastErr error
	for _, m := range []string{want, now.UTC().AddDate(0, -1, -now.UTC().Day()+1).Format("2006-01")} {
		if m == month {
			break // the previous month is what we have
		}
		b, err := sys.fetch(ctx, geoURL(m))
		if err == nil {
			err = checkGeoList(b)
		}
		if err != nil {
			lastErr = err
			continue
		}
		tmp := geoList + ".new"
		if err := os.WriteFile(sys.path(tmp), b, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(sys.path(tmp), sys.path(geoList)); err != nil {
			return "", err
		}
		if err := os.WriteFile(sys.path(geoMonth), []byte(m+"\n"), 0o644); err != nil {
			return "", err
		}
		return m, nil
	}
	if month != "" {
		return month, nil // keep using the list we have
	}
	return "", fmt.Errorf("download the country list: %v", lastErr)
}

// checkGeoList makes sure b is a gzipped country list: rows of start,end,country.
func checkGeoList(b []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("the country list is not gzip: %w", err)
	}
	rows := 0
	err = readGeo(zr, func(start, end netip.Addr, cc string) bool {
		rows++
		return true
	})
	if err == nil && rows < geoMinRows {
		err = fmt.Errorf("the country list has only %d rows", rows)
	}
	return err
}

// readGeo calls fn for every row of a country list until it returns false.
func readGeo(r io.Reader, fn func(start, end netip.Addr, cc string) bool) error {
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		f := strings.Split(sc.Text(), ",")
		if len(f) != 3 {
			return fmt.Errorf("country list line %d: want 3 fields", n)
		}
		start, err1 := netip.ParseAddr(f[0])
		end, err2 := netip.ParseAddr(f[1])
		cc := f[2]
		if err1 != nil || err2 != nil || start.Is4() != end.Is4() || end.Less(start) || len(cc) != 2 || strings.ToUpper(cc) != cc {
			return fmt.Errorf("country list line %d is not start,end,country", n)
		}
		if !fn(start, end, cc) {
			return nil
		}
	}
	return sc.Err()
}

type addrRange struct{ start, end netip.Addr }

func (r addrRange) String() string {
	if r.start == r.end {
		return r.start.String()
	}
	return r.start.String() + "-" + r.end.String()
}

// countryRanges reads the ranges of the given countries, joining neighbours.
func countryRanges(path string, codes []string) (v4, v6 []addrRange, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	err = readGeo(zr, func(start, end netip.Addr, cc string) bool {
		if !slices.Contains(codes, cc) {
			return true
		}
		list := &v6
		if start.Is4() {
			list = &v4
		}
		if n := len(*list); n > 0 && (*list)[n-1].end.Next() == start {
			(*list)[n-1].end = end
		} else {
			*list = append(*list, addrRange{start, end})
		}
		return true
	})
	return v4, v6, err
}

// countryOf looks an address up in the country list.
func countryOf(path string, ip netip.Addr) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	ip = ip.Unmap()
	found := ""
	err = readGeo(zr, func(start, end netip.Addr, cc string) bool {
		if start.Is4() == ip.Is4() && !ip.Less(start) && !end.Less(ip) {
			found = cc
			return false
		}
		return true
	})
	if found == "ZZ" {
		found = ""
	}
	return found, err
}

// mergePrefixes turns addresses and networks into sorted ranges without overlaps.
func mergePrefixes(entries []string) (v4, v6 []addrRange) {
	var all []addrRange
	for _, e := range entries {
		p, err := netip.ParsePrefix(e)
		if err != nil {
			a, err := netip.ParseAddr(e)
			if err != nil {
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		p = p.Masked()
		all = append(all, addrRange{p.Addr(), lastAddr(p)})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start.Less(all[j].start) })
	for _, r := range all {
		list := &v6
		if r.start.Is4() {
			list = &v4
		}
		if n := len(*list); n > 0 {
			last := &(*list)[n-1]
			if !last.end.Next().IsValid() || !last.end.Next().Less(r.start) {
				if last.end.Less(r.end) {
					last.end = r.end
				}
				continue
			}
		}
		*list = append(*list, r)
	}
	return v4, v6
}

// lastAddr is the highest address of a network.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 0x80 >> (i % 8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

// countriesConfig is the nftables table: replaced as a whole, in one transaction.
func countriesConfig(key, mode string, v4, v6 []addrRange, exempt []string) []byte {
	e4, e6 := mergePrefixes(exempt)
	var b strings.Builder
	fmt.Fprintf(&b, "# ekPanel country blocking. Managed by ekPanel; changes here are overwritten.\n# key: %s\n# ranges: %d\n", key, len(v4)+len(v6))
	fmt.Fprintf(&b, "table inet %[1]s\ndelete table inet %[1]s\ntable inet %[1]s {\n", countriesTable)
	// A set's own "elements =" line may appear once (a second replaces the first), so the
	// ranges are added after the table, in chunks, in the same transaction.
	var adds strings.Builder
	set := func(name, typ string, rs []addrRange) {
		fmt.Fprintf(&b, "\tset %s {\n\t\ttype %s\n\t\tflags interval\n\t}\n", name, typ)
		for i := 0; i < len(rs); i += 500 {
			part := rs[i:min(i+500, len(rs))]
			strs := make([]string, len(part))
			for j, r := range part {
				strs[j] = r.String()
			}
			fmt.Fprintf(&adds, "add element inet %s %s { %s }\n", countriesTable, name, strings.Join(strs, ", "))
		}
	}
	set("exempt4", "ipv4_addr", e4)
	set("exempt6", "ipv6_addr", e6)
	set("countries4", "ipv4_addr", v4)
	set("countries6", "ipv6_addr", v6)
	b.WriteString(`	chain prerouting {
		type filter hook prerouting priority -150; policy accept;
		ct state established,related accept
		iif "lo" accept
		# Certificates renew over port 80, which only redirects to HTTPS.
		tcp dport 80 accept
		udp sport 67 udp dport 68 accept
		udp sport 547 udp dport 546 accept
		icmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-advert, nd-redirect } accept
		ip saddr @exempt4 accept
		ip6 saddr @exempt6 accept
`)
	if mode == agentproto.CountriesAllow {
		b.WriteString("\t\tip saddr @countries4 accept\n\t\tip6 saddr @countries6 accept\n\t\tdrop\n")
	} else {
		b.WriteString("\t\tip saddr @countries4 drop\n\t\tip6 saddr @countries6 drop\n")
	}
	b.WriteString("\t}\n}\n")
	b.WriteString(adds.String())
	return []byte(b.String())
}

// securityCountryOf answers which country an address is in, downloading the list first
// if needed.
func securityCountryOf(ctx context.Context, sys *system, p agentproto.CountryOfParams) (any, *agentproto.OpError) {
	ip, err := netip.ParseAddr(p.IP)
	if err != nil {
		return nil, opError(agentproto.ErrInvalidParams, "invalid address")
	}
	if _, err := ensureGeoList(ctx, sys, time.Now()); err != nil {
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	cc, err := countryOf(sys.path(geoList), ip)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, opError(agentproto.ErrFailed, "the country list is missing")
		}
		return nil, opError(agentproto.ErrFailed, "%v", err)
	}
	return agentproto.CountryOfResult{Country: cc}, nil
}
