package agentproto

import (
	"reflect"
	"strings"
	"testing"
)

// A zone as Stalwart 1.0.3 writes it for a domain.
const stalwartZone = `v1-ed25519-20261107._domainkey.php.test. IN TXT "v=DKIM1; k=ed25519; h=sha256; p=5+B3PeJlwqs/9g3EQQoTG/NRmrWPgWEIzQ1VCMoUGsM="
v1-rsa-20261107._domainkey.php.test. IN TXT (
    "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkq"
    "tEwxy50ztLhV2WdVBx"
)
php.test. IN TXT "v=spf1 mx -all"
php.test. IN MX 10 mail.example.net.
_dmarc.php.test. IN TXT "v=DMARC1; p=reject; rua=mailto:postmaster@php.test"
_caldavs._tcp.php.test. IN SRV 0 1 443 mail.example.net.
_imaps._tcp.php.test. IN SRV 0 1 993 mail.example.net.
mta-sts.php.test. IN CNAME mail.example.net.
_mta-sts.php.test. IN TXT "v=STSv1; id=138449342089700316"
_smtp._tls.php.test. IN TXT "v=TLSRPTv1; rua=mailto:postmaster@php.test"
autoconfig.php.test. IN CNAME mail.example.net.
`

func TestParseMailZone(t *testing.T) {
	got, err := ParseMailZone("php.test", stalwartZone)
	if err != nil {
		t.Fatal(err)
	}
	want := []MailDNSRecord{
		{Type: "TXT", Name: "v1-ed25519-20261107._domainkey.php.test", Content: "v=DKIM1; k=ed25519; h=sha256; p=5+B3PeJlwqs/9g3EQQoTG/NRmrWPgWEIzQ1VCMoUGsM="},
		{Type: "TXT", Name: "v1-rsa-20261107._domainkey.php.test", Content: "v=DKIM1; k=rsa; h=sha256; p=MIIBIjANBgkqtEwxy50ztLhV2WdVBx"},
		{Type: "TXT", Name: "php.test", Content: "v=spf1 mx -all"},
		{Type: "MX", Name: "php.test", Content: "mail.example.net", Priority: 10},
		{Type: "TXT", Name: "_dmarc.php.test", Content: "v=DMARC1; p=reject; rua=mailto:postmaster@php.test"},
		{Type: "TXT", Name: "_smtp._tls.php.test", Content: "v=TLSRPTv1; rua=mailto:postmaster@php.test"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if _, err := ParseMailZone("other.test", stalwartZone); err == nil {
		t.Fatal("records of another domain must be refused")
	}
	if _, err := ParseMailZone("php.test", "php.test. IN TXT (\n\"v=spf1\"\n"); err == nil {
		t.Fatal("an unbalanced record must be refused")
	}
}

func TestValidateMailDomain(t *testing.T) {
	hash := "$6$saltsalt$" + strings.Repeat("a", 86)
	id := func(n byte) string { return "00000000-0000-0000-0000-0000000000" + string([]byte{'0', n}) }
	good := MailDomainSpec{ID: id('1'), Domain: "example.com", CatchAll: "info",
		Mailboxes: []MailboxSpec{{ID: id('2'), Name: "info", PasswordHash: hash, KeepCopy: true, QuotaMB: 500,
			ForwardTo: []string{"me@other.org"}, AutoReply: &AutoReply{Subject: "Away", Body: "Back Monday", Start: "2026-11-01", End: "2026-11-30"}}},
		Forwarders: []ForwarderSpec{{ID: id('3'), Name: "sales", To: []string{"info@example.com", "boss@other.org"}}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(d *MailDomainSpec){
		"bad domain":          func(d *MailDomainSpec) { d.Domain = "Example..com" },
		"uppercase name":      func(d *MailDomainSpec) { d.Mailboxes[0].Name = "Info" },
		"dots":                func(d *MailDomainSpec) { d.Mailboxes[0].Name = "a..b" },
		"plain password":      func(d *MailDomainSpec) { d.Mailboxes[0].PasswordHash = "secret" },
		"negative quota":      func(d *MailDomainSpec) { d.Mailboxes[0].QuotaMB = -1 },
		"name twice":          func(d *MailDomainSpec) { d.Forwarders[0].Name = "info" },
		"forward with header": func(d *MailDomainSpec) { d.Forwarders[0].To = []string{"a@b.org\r\nBcc: x@y.org"} },
		"named address":       func(d *MailDomainSpec) { d.Forwarders[0].To = []string{"Bob <bob@b.org>"} },
		"keeps nothing":       func(d *MailDomainSpec) { d.Mailboxes[0].KeepCopy, d.Mailboxes[0].ForwardTo = false, nil },
		"missing catch-all":   func(d *MailDomainSpec) { d.CatchAll = "nobody" },
		"reply backwards":     func(d *MailDomainSpec) { d.Mailboxes[0].AutoReply.End = "2026-10-01" },
		"two-line subject":    func(d *MailDomainSpec) { d.Mailboxes[0].AutoReply.Subject = "a\nb" },
		"empty forwarder":     func(d *MailDomainSpec) { d.Forwarders[0].To = nil },
	}
	for name, change := range cases {
		d := good
		d.Mailboxes = []MailboxSpec{good.Mailboxes[0]}
		reply := *good.Mailboxes[0].AutoReply
		d.Mailboxes[0].AutoReply = &reply
		d.Forwarders = []ForwarderSpec{good.Forwarders[0]}
		change(&d)
		if d.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
