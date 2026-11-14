package agentproto

import (
	"fmt"
	"slices"
	"testing"
)

func TestParseZoneFile(t *testing.T) {
	zone := `; cPanel first:11.110.0.17 (update_time):1730000000
; Zone file for shop.com
$TTL 14400
shop.com.	86400	IN	SOA	ns1.host.net.	admin.host.net.	(
						2024110301 ;Serial Number
						3600 ;refresh
						7200 ;retry
						1209600 ;expire
						86400 ;minimum
	)
shop.com.	86400	IN	NS	ns1.host.net.
shop.com.	14400	IN	A	203.0.113.5
www	IN	CNAME	shop.com.
mail	14400	IN	CNAME	shop.com.
shop.com.	IN	MX	0	shop.com.
shop.com.	IN	TXT	"v=spf1 +a +mx ~all"
default._domainkey	14400	IN	TXT	( "v=DKIM1; k=rsa; "
	"p=MIIBIjAN" "BgkqhkiG" )
_dmarc	IN	TXT	"v=DMARC1; p=none; rua=mailto:a\"b@shop.com"
@	3600	IN	CAA	0 issue "letsencrypt.org"
_sip._tcp	IN	SRV	10 5 5060 sip.shop.com.
api	1h	IN	A	198.51.100.7
	IN	AAAA	2001:db8::7
$ORIGIN sub.shop.com.
x	IN	A	192.0.2.1
odd	IN	PTR	nowhere.
broken	IN	MX	high
`
	recs, unread := ParseZoneFile(zone, "shop.com")
	var got []string
	for _, r := range recs {
		got = append(got, fmt.Sprintf("%s %d %s %d %s", r.Name, r.TTL, r.Type, r.Priority, r.Content))
	}
	want := []string{
		"shop.com 86400 NS 0 ns1.host.net",
		"shop.com 14400 A 0 203.0.113.5",
		"www.shop.com 14400 CNAME 0 shop.com",
		"mail.shop.com 14400 CNAME 0 shop.com",
		"shop.com 14400 MX 0 shop.com",
		"shop.com 14400 TXT 0 v=spf1 +a +mx ~all",
		"default._domainkey.shop.com 14400 TXT 0 v=DKIM1; k=rsa; p=MIIBIjANBgkqhkiG",
		`_dmarc.shop.com 14400 TXT 0 v=DMARC1; p=none; rua=mailto:a"b@shop.com`,
		`shop.com 3600 CAA 0 0 issue "letsencrypt.org"`,
		"_sip._tcp.shop.com 14400 SRV 10 5 5060 sip.shop.com",
		"api.shop.com 3600 A 0 198.51.100.7",
		"api.shop.com 14400 AAAA 0 2001:db8::7",
		"x.sub.shop.com 14400 A 0 192.0.2.1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("records:\n%q\nwant\n%q", got, want)
	}
	if unread != 2 {
		t.Fatalf("unread %d, want 2 (PTR and the broken MX)", unread)
	}
	for _, r := range recs {
		if r.Type == "NS" {
			continue
		}
		if err := r.Check("shop.com"); err != nil {
			t.Errorf("%+v does not pass Check: %v", r, err)
		}
	}
}
