package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"ekpanel/internal/agentproto"
)

// testGeoList is a country list of 120,000 /24 ranges from 1.0.0.0, cycling through BD,
// BD, US and CN, then one IPv6 range for BD and the rest unknown.
func testGeoList(t *testing.T) []byte {
	t.Helper()
	var csv strings.Builder
	a := netip.MustParseAddr("1.0.0.0")
	ccs := []string{"BD", "BD", "US", "CN"}
	for i := range 120_000 {
		b := a.As4()
		end := netip.AddrFrom4([4]byte{b[0], b[1], b[2], 255})
		fmt.Fprintf(&csv, "%s,%s,%s\n", a, end, ccs[i%4])
		a = end.Next()
	}
	csv.WriteString("2001:db8::,2001:db8:ffff:ffff:ffff:ffff:ffff:ffff,BD\n2001:db9::,2001:db9::ffff,ZZ\n")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(csv.String()))
	zw.Close()
	return buf.Bytes()
}

func TestGeoListDownload(t *testing.T) {
	f := newFakeSystem(t)
	list := testGeoList(t)
	var asked []string
	f.system.fetch = func(_ context.Context, url string) ([]byte, error) {
		asked = append(asked, url)
		if strings.Contains(url, "2026-11") {
			return nil, errors.New("GET " + url + ": 404 Not Found")
		}
		return list, nil
	}
	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	month, err := ensureGeoList(context.Background(), f.system, now)
	if err != nil || month != "2026-10" || len(asked) != 2 || !strings.HasSuffix(asked[1], "dbip-country-lite-2026-10.csv.gz") {
		t.Fatalf("early in the month the previous list is used: %q %v %v", month, err, asked)
	}
	// Within a day nothing is downloaded again; a day later the new month is tried.
	if month, _ := ensureGeoList(context.Background(), f.system, now.Add(time.Hour)); month != "2026-10" || len(asked) != 2 {
		t.Errorf("asked again too soon: %v", asked)
	}
	f.system.fetch = func(_ context.Context, url string) ([]byte, error) {
		asked = append(asked, url)
		return []byte("not gzip"), nil
	}
	if month, err := ensureGeoList(context.Background(), f.system, now.Add(25*time.Hour)); month != "2026-10" || err != nil || len(asked) != 3 {
		t.Errorf("a broken download must keep the list we have: %q %v %v", month, err, asked)
	}

	cc, err := countryOf(f.path(geoList), netip.MustParseAddr("1.0.2.7"))
	if err != nil || cc != "US" {
		t.Errorf("1.0.2.7: %q %v", cc, err)
	}
	if cc, _ := countryOf(f.path(geoList), netip.MustParseAddr("2001:db9::1")); cc != "" {
		t.Errorf("ZZ must read as unknown, got %q", cc)
	}
	if cc, _ := countryOf(f.path(geoList), netip.MustParseAddr("::ffff:1.0.0.1")); cc != "BD" {
		t.Errorf("a mapped IPv4 address: %q", cc)
	}
}

func TestApplyCountries(t *testing.T) {
	f := newFakeSystem(t)
	list := testGeoList(t)
	f.system.fetch = func(context.Context, string) ([]byte, error) { return list, nil }
	loaded := false
	run := f.system.run
	f.system.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "nft" {
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			switch args[0] {
			case "-f":
				loaded = true
			case "delete":
				loaded = false
			case "list":
				if loaded {
					return []byte("table ip filter\ntable inet " + countriesTable + "\n"), nil
				}
				return []byte("table ip filter\n"), nil
			}
			return nil, nil
		}
		return run(ctx, name, args...)
	}
	spec := agentproto.CountryBlock{Mode: agentproto.CountriesBlock, Countries: []string{"CN", "BD"}}
	st := applyCountries(context.Background(), f.system, spec, []string{"203.0.113.9", "198.51.100.0/24"})
	if !st.Active || st.Error != "" || st.List == "" {
		t.Fatalf("status: %+v", st)
	}
	conf := f.read(t, countriesNFT)
	for _, want := range []string{
		"type filter hook prerouting priority -150; policy accept;",
		"ct state established,related accept",
		"tcp dport 80 accept",
		"ip saddr @countries4 drop",
		"203.0.113.9", "198.51.100.0-198.51.100.255", "10.0.0.0-10.255.255.255", "2606:4700::-2606:4700:ffff:ffff:ffff:ffff:ffff:ffff",
		// Neighbouring ranges are one: BD's two, and CN's followed by BD's.
		"1.0.0.0-1.0.1.255, 1.0.3.0-1.0.5.255, 1.0.7.0-1.0.9.255",
		"2001:db8::-2001:db8:ffff:ffff:ffff:ffff:ffff:ffff",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	// nft keeps only the last "elements =" of a set, so ranges come in "add element" lines.
	if strings.Contains(conf, "elements =") || strings.Count(conf, "add element inet "+countriesTable+" countries4 {") != 61 {
		t.Error("the ranges must be added in chunks of 500, after the sets")
	}
	if strings.Contains(conf, "1.0.2.0") || strings.Contains(conf, "\t\tdrop\n") {
		t.Error("US must not be blocked, and nothing else dropped")
	}
	if want := 120_000/4 + 1 + 1; st.Ranges != want {
		t.Errorf("ranges = %d, want %d", st.Ranges, want)
	}
	if !f.exists(countriesUnit) || !f.ran("systemctl enable ekpanel-countries") {
		t.Error("the rules must load again at boot")
	}

	// Unchanged: not loaded again.
	f.calls = nil
	if st := applyCountries(context.Background(), f.system, spec, []string{"198.51.100.0/24", "203.0.113.9"}); !st.Active || f.ran("nft -f") {
		t.Errorf("unchanged rules were loaded again: %+v %v", st, f.calls)
	}
	// Allow mode drops everything else.
	f.calls = nil
	applyCountries(context.Background(), f.system, agentproto.CountryBlock{Mode: agentproto.CountriesAllow, Countries: []string{"BD"}}, nil)
	conf = f.read(t, countriesNFT)
	if !f.ran("nft -f") || !strings.Contains(conf, "ip saddr @countries4 accept\n\t\tip6 saddr @countries6 accept\n\t\tdrop\n") {
		t.Errorf("allow mode:\n%s", conf)
	}
	// A bad spec keeps the loaded rules and says so.
	if st := applyCountries(context.Background(), f.system, agentproto.CountryBlock{Mode: agentproto.CountriesBlock, Countries: []string{"XX"}}, nil); st.Error == "" || !st.Active {
		t.Errorf("bad spec: %+v", st)
	}
	// Off removes the table, the file and the unit.
	if st := applyCountries(context.Background(), f.system, agentproto.CountryBlock{Mode: agentproto.CountriesOff}, nil); st.Active || loaded ||
		f.exists(countriesNFT) || f.exists(countriesUnit) {
		t.Errorf("off: %+v loaded=%v", st, loaded)
	}
}

func TestMergePrefixes(t *testing.T) {
	v4, v6 := mergePrefixes([]string{"10.0.0.0/8", "10.1.0.0/16", "11.0.0.0/8", "192.0.2.1", "192.0.2.2", "bad", "::1/128", "fc00::/7"})
	got := fmt.Sprint(v4, v6)
	if got != "[10.0.0.0-11.255.255.255 192.0.2.1-192.0.2.2] [::1 fc00::-fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]" {
		t.Errorf("merged: %s", got)
	}
	if v4, _ := mergePrefixes([]string{"255.255.255.0/24", "255.255.255.255"}); len(v4) != 1 {
		t.Errorf("the last network: %v", v4)
	}
}

func TestCountryBlock(t *testing.T) {
	for _, bad := range []agentproto.CountryBlock{
		{Mode: "deny", Countries: []string{"CN"}},
		{Mode: agentproto.CountriesBlock},
		{Mode: agentproto.CountriesBlock, Countries: []string{"cn"}},
		{Mode: agentproto.CountriesBlock, Countries: []string{"CN", "CN"}},
		{Mode: agentproto.CountriesAllow, Countries: []string{"ZZ"}},
	} {
		if bad.Validate() == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	block := agentproto.CountryBlock{Mode: agentproto.CountriesBlock, Countries: []string{"CN"}}
	allow := agentproto.CountryBlock{Mode: agentproto.CountriesAllow, Countries: []string{"BD"}}
	if !block.Blocks("CN") || block.Blocks("BD") || block.Blocks("") || allow.Blocks("BD") || !allow.Blocks("US") || !allow.Blocks("") {
		t.Error("Blocks")
	}
}
