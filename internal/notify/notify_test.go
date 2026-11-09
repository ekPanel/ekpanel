package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ekpanel/internal/store"
)

func TestRender(t *testing.T) {
	n := store.Notification{Event: "server_offline", Params: map[string]string{"server": "web-1", "minutes": "5"}, CreatedAt: time.Now()}
	if m := Render("bn", n); m.Title != "সার্ভার web-1 অফলাইন" || !strings.Contains(m.Body, "৫") && !strings.Contains(m.Body, "5") {
		t.Errorf("bn: %+v", m)
	}
	if m := Render("xx", n); m.Title != "Server web-1 is offline" {
		t.Errorf("fallback: %+v", m)
	}
	for _, e := range append(AdminEvents, "test") {
		for l, c := range catalogs {
			if c.Events[e].Title == "" || c.Events[e].Body == "" {
				t.Errorf("%s has no message for %s", l, e)
			}
		}
	}
}

func TestPrivateAddressesBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	err := postRaw(context.Background(), httpClient(false), srv.URL, []byte("{}"), nil)
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("a customer's webhook reached %s: %v", srv.URL, err)
	}
	if err := postRaw(context.Background(), httpClient(true), srv.URL, []byte("{}"), nil); err != nil {
		t.Fatalf("an administrator's local webhook: %v", err)
	}
	for _, ip := range []string{"10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1"} {
		if !privateIP(net.ParseIP(ip)) {
			t.Errorf("%s counted as public", ip)
		}
	}
	for _, ip := range []string{"1.1.1.1", "203.0.113.9", "2606:4700::1111"} {
		if privateIP(net.ParseIP(ip)) {
			t.Errorf("%s counted as private", ip)
		}
	}
}
