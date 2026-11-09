package tasks

import (
	"slices"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestCPanelCronPHP(t *testing.T) {
	for in, want := range map[string]string{
		"/usr/local/bin/php /home/u/cron.php":                        "php8.2 /home/u/cron.php",
		"/opt/cpanel/ea-php74/root/usr/bin/php -q /home/u/x.php":     "php8.2 -q /home/u/x.php",
		"cd /home/u && /usr/local/bin/ea-php81 artisan schedule:run": "cd /home/u && php8.2 artisan schedule:run",
		"php /home/u/wp-cron.php":                                    "php8.2 /home/u/wp-cron.php",
		"wget -q -O- https://x.test/php >/dev/null":                  "wget -q -O- https://x.test/php >/dev/null",
		"/home/u/bin/php-worker.sh":                                  "/home/u/bin/php-worker.sh",
		"(php -v)":                                                   "(php8.2 -v)",
	} {
		if got := cpanelPHPRE.ReplaceAllString(in, "${1}php8.2${2}"); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
	sched, cmd, ok := splitCronLine("*/5  * * * *   php  /x.php   a")
	if !ok || sched != "*/5 * * * *" || cmd != "php  /x.php   a" {
		t.Fatalf("split: %q %q %v", sched, cmd, ok)
	}
	if _, _, ok := splitCronLine("@daily"); ok {
		t.Fatal("a schedule without a command")
	}
}

func TestFilterCPanelZone(t *testing.T) {
	zone := `$TTL 14400
shop.com. IN NS ns1.oldhost.net.
shop.com. IN A 203.0.113.5
www IN CNAME shop.com.
mail IN CNAME shop.com.
ftp IN A 203.0.113.5
cpanel IN A 203.0.113.5
webmail IN A 203.0.113.5
_cpanel-dcv-test-record IN TXT "abc"
_autodiscover._tcp IN SRV 0 0 443 cpanelemaildiscovery.cpanel.net.
shop.com. IN MX 0 shop.com.
shop.com. IN TXT "v=spf1 +a +mx ~all"
shop.com. IN TXT "google-site-verification=xyz"
default._domainkey IN TXT "v=DKIM1; k=rsa; p=AAA"
_dmarc IN TXT "v=DMARC1; p=none"
blog IN CNAME shops.example.net.
api IN A 198.51.100.7
api IN A 198.51.100.7
bad IN A not-an-ip
odd IN PTR x.
`
	recs, unread := agentproto.ParseZoneFile(zone, "shop.com")
	z := agentproto.CPanelZone{Name: "shop.com", Records: recs, Unread: unread}
	show := func(rs []agentproto.HostedRecord) (out []string) {
		for _, r := range rs {
			out = append(out, r.Name+" "+r.Type+" "+r.Content)
		}
		return
	}
	managed := map[string]bool{"shop.com": true, "www.shop.com": true}

	// Email moves here too: ekPanel makes the mail records.
	keep, notes := FilterCPanelZone(z, CPanelZoneFilter{Managed: managed, MailHere: true, OldIP: "203.0.113.5"})
	want := []string{"mail.shop.com CNAME shop.com", "ftp.shop.com A 203.0.113.5", "shop.com TXT google-site-verification=xyz",
		"blog.shop.com CNAME shops.example.net", "api.shop.com A 198.51.100.7"}
	if !slices.Equal(show(keep), want) {
		t.Fatalf("kept %q\nwant %q", show(keep), want)
	}
	codes := func(ns []ImportNote) (out []string) {
		for _, n := range ns {
			out = append(out, n.Code+" "+n.Name)
		}
		return
	}
	if got := codes(notes); !slices.Equal(got, []string{"zone_record bad.shop.com A not-an-ip", "zone_old_ip ftp.shop.com", "zone_unread shop.com"}) {
		t.Fatalf("notes %q", got)
	}

	// Email stays at the old host: its records stay, and an MX at the domain itself is
	// pointed out (the domain now points here).
	keep, notes = FilterCPanelZone(z, CPanelZoneFilter{Managed: managed, OldIP: "203.0.113.5"})
	if !slices.Contains(show(keep), "shop.com MX shop.com") || !slices.Contains(show(keep), "default._domainkey.shop.com TXT v=DKIM1; k=rsa; p=AAA") {
		t.Fatalf("kept %q", show(keep))
	}
	if got := codes(notes); !slices.Contains(got, "zone_mx_here shop.com") {
		t.Fatalf("notes %q", got)
	}
}
