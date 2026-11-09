package cloudflare_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"ekpanel/internal/cloudflare"
	"ekpanel/internal/cloudflare/cloudflaretest"
)

func TestClient(t *testing.T) {
	fake := cloudflaretest.New("good")
	srv := httptest.NewServer(fake)
	defer srv.Close()
	ctx := context.Background()
	c := &cloudflare.Client{Token: "good", Base: srv.URL, HTTP: srv.Client()}

	if err := (&cloudflare.Client{Token: "bad", Base: srv.URL, HTTP: srv.Client()}).Verify(ctx); !cloudflare.IsAuth(err) {
		t.Fatalf("bad token: %v", err)
	}
	if err := c.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	zid := fake.AddZone("example.com")
	zones, err := c.Zones(ctx)
	if err != nil || len(zones) != 1 || zones[0].Name != "example.com" {
		t.Fatalf("zones: %v %+v", err, zones)
	}
	rec, err := c.CreateRecord(ctx, zid, cloudflare.Record{Type: "A", Name: "example.com", Content: "203.0.113.5", TTL: 1, Proxied: true, Comment: cloudflare.ManagedComment})
	if err != nil || rec.ID == "" {
		t.Fatalf("create: %v %+v", err, rec)
	}
	if _, err := c.CreateRecord(ctx, zid, cloudflare.Record{Type: "CNAME", Name: "example.com", Content: "x.example.net"}); err == nil {
		t.Error("CNAME next to an A record accepted")
	}
	rec.Content = "203.0.113.6"
	if _, err := c.UpdateRecord(ctx, zid, rec); err != nil {
		t.Fatal(err)
	}
	list, err := c.Records(ctx, zid)
	if err != nil || len(list) != 1 || list[0].Content != "203.0.113.6" || !list[0].Proxied {
		t.Fatalf("records: %v %+v", err, list)
	}
	if err := c.DeleteRecord(ctx, zid, rec.ID); err != nil || len(fake.Records(zid)) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := c.SetSSLMode(ctx, zid, "strict"); err != nil {
		t.Fatal(err)
	}
	if mode, err := c.SSLMode(ctx, zid); err != nil || mode != "strict" {
		t.Fatalf("ssl: %v %s", err, mode)
	}
	if err := c.PurgeCache(ctx, zid); err != nil || fake.Purges(zid) != 1 {
		t.Fatalf("purge: %v", err)
	}
	z, err := c.CreateZone(ctx, "acc1", "new.example")
	if err != nil || z.Status != "pending" || len(z.NameServers) != 2 {
		t.Fatalf("create zone: %v %+v", err, z)
	}
	if _, err := c.Zone(ctx, "nope"); err == nil {
		t.Error("unknown zone found")
	}
}
