package api

import (
	"strings"
	"testing"
)

func TestNormalizeHostname(t *testing.T) {
	cases := map[string]string{
		"Example.COM":       "example.com",
		"shop.example.com.": "shop.example.com",
		" www.example.com ": "www.example.com",
		"উদাহরণ.বাংলা":      "xn--d5b6ci4b4b3a.xn--54b7fta0cc",
		"bücher.de":         "xn--bcher-kva.de",
		"localhost":         "",
		"1.2.3.4":           "",
		"*.example.com":     "",
		"a b.com":           "",
		"evil.com {":        "",
		"http://x.com":      "",
		"-bad.com":          "",
	}
	for in, want := range cases {
		if got := normalizeHostname(in); got != want {
			t.Errorf("normalizeHostname(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinuxUserFor(t *testing.T) {
	for domain, prefix := range map[string]string{
		"my-shop.example.com":             "myshop_",
		"averyveryverylongname.com":       "averyveryver_",
		"123.example.com":                 "site123_",
		"xn--d5b6ci4b4b3a.xn--54b7fta0cc": "d5b6ci4b4b3a_",
	} {
		got := linuxUserFor(domain)
		if !strings.HasPrefix(got, prefix) || len(got) != len(prefix)+4 || len(got) > 32 {
			t.Errorf("linuxUserFor(%q) = %q, want %q + 4 hex", domain, got, prefix)
		}
	}
}
