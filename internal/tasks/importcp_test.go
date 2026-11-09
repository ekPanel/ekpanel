package tasks

import "testing"

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
