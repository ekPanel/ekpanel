package tasks

import (
	"testing"

	"ekpanel/internal/store"
)

func TestPublicAddress(t *testing.T) {
	set := "198.51.100.9"
	for _, c := range []struct {
		target store.HostTarget
		v4, v6 string
	}{
		{store.HostTarget{Addresses: []string{"10.0.2.15", "100.64.1.2", "172.17.0.1", "203.0.113.4", "fd00::1", "2001:db8::4"}}, "203.0.113.4", "2001:db8::4"},
		{store.HostTarget{Addresses: []string{"10.0.2.15", "192.168.1.5", "fec0::5054:ff:fe12:3456"}}, "", ""},
		{store.HostTarget{Addresses: []string{"203.0.113.4"}, PublicIPv4: &set}, "198.51.100.9", ""},
	} {
		if v4, v6 := publicAddress(c.target, "A"), publicAddress(c.target, "AAAA"); v4 != c.v4 || v6 != c.v6 {
			t.Errorf("%+v: %q %q, want %q %q", c.target, v4, v6, c.v4, c.v6)
		}
	}
}

func TestZoneFor(t *testing.T) {
	cust, other := "c1", "c2"
	zones := []store.DNSZone{{ID: "root", Name: "example.com"}, {ID: "sub", Name: "shop.example.com"}, {ID: "theirs", Name: "customer.net", OwnerID: &cust}}
	for _, c := range []struct {
		host  string
		owner *string
		want  string
	}{
		{"example.com", nil, "root"},
		{"www.example.com", &cust, "root"}, // the administrators' zones hold customers' sites too
		{"a.shop.example.com", nil, "sub"}, // the most specific zone
		{"badexample.com", nil, ""},
		{"blog.customer.net", &cust, "theirs"},
		{"blog.customer.net", &other, ""}, // not someone else's zone
		{"blog.customer.net", nil, ""},
	} {
		z := ZoneFor(c.host, c.owner, zones)
		got := ""
		if z != nil {
			got = z.ID
		}
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.host, got, c.want)
		}
	}
}
