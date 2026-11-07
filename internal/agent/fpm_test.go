package agent

import (
	"context"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

const ownerID = "abcdef01-2345-6789-abcd-ef0123456789"

func applyWithOwners(t *testing.T, f *fakeSystem, owners []agentproto.OwnerSpec, sites ...agentproto.SiteSpec) []agentproto.SiteResult {
	t.Helper()
	f.calls = nil
	res, opErr := stateApply(context.Background(), f.system, agentproto.StateApplyParams{Sites: sites, Owners: owners})
	if opErr != nil {
		t.Fatal(opErr)
	}
	return res.Sites
}

func TestCustomerSiteRunsInItsSlice(t *testing.T) {
	f := newFakeSystem(t)
	key := ownerKey(ownerID)
	unit := "ekpanel-php8.4-c" + key + ".service"
	owners := []agentproto.OwnerSpec{{ID: ownerID, MemoryMB: 512, CPUPercent: 50}}
	sp := phpSite()
	sp.Owner = ownerID
	if res := applyWithOwners(t, f, owners, sp); !res[0].OK {
		t.Fatalf("apply: %s", res[0].Error)
	}

	slice := f.read(t, systemdDir+"/ekpanel-c"+key+".slice")
	if !strings.Contains(slice, "MemoryMax=512M") || !strings.Contains(slice, "CPUQuota=50%") {
		t.Errorf("slice:\n%s", slice)
	}
	if !f.ran("systemctl set-property --runtime ekpanel-c" + key + ".slice MemoryMax=512M CPUQuota=50%") {
		t.Errorf("limits not applied to the running slice: %v", f.calls)
	}
	g := fpmGroup{version: "8.4", key: key}
	pool := f.read(t, g.poolPath(siteID))
	if !strings.Contains(pool, "listen = "+g.socket(siteID)) || !strings.Contains(pool, "user = example_ab12") {
		t.Errorf("pool:\n%s", pool)
	}
	if f.exists(poolPath("8.4", siteID)) {
		t.Error("a customer's site must not have a pool in the system's PHP-FPM")
	}
	u := f.read(t, systemdDir+"/"+unit)
	if !strings.Contains(u, "Slice=ekpanel-c"+key+".slice") || !strings.Contains(u, "OOMPolicy=continue") || !strings.Contains(u, "--fpm-config "+g.dir()+"/php-fpm.conf") {
		t.Errorf("unit:\n%s", u)
	}
	if !strings.Contains(f.read(t, g.dir()+"/php-fpm.conf"), "include = "+g.poolDir()+"/*.conf") {
		t.Error("master does not include the customer's pools")
	}
	if !f.ran("systemctl daemon-reload") || !f.ran("systemctl enable "+unit) || !f.ran("systemctl reload-or-restart "+unit) {
		t.Errorf("master not started: %v", f.calls)
	}
	if !strings.Contains(f.read(t, caddySitePath(siteID)), "php_fastcgi unix/"+g.socket(siteID)) {
		t.Error("Caddy does not use the customer's socket")
	}

	// Unchanged: no reloads.
	applyWithOwners(t, f, owners, sp)
	if f.ran("systemctl reload-or-restart") || f.ran("systemctl set-property") {
		t.Errorf("unchanged apply reloaded: %v", f.calls)
	}

	// The site goes back to the administrators: its pool moves, the empty master stops,
	// and the slice of a customer with nothing left goes.
	sp.Owner = ""
	applyWithOwners(t, f, nil, sp)
	if !f.exists(poolPath("8.4", siteID)) || f.exists(g.poolPath(siteID)) {
		t.Fatal("pool did not move to the system's PHP-FPM")
	}
	if !f.ran("systemctl reload-or-restart php8.4-fpm.service") || !f.ran("systemctl disable --now "+unit) {
		t.Errorf("masters not switched: %v", f.calls)
	}
	if f.exists(systemdDir+"/"+unit) || f.exists(systemdDir+"/ekpanel-c"+key+".slice") {
		t.Error("the customer's unit and slice must be removed")
	}
}

func TestCustomerAppGetsSlice(t *testing.T) {
	ap := imageApp()
	ap.Owner = ownerID
	if !strings.Contains(string(imageCompose(ap)), `"cgroup_parent": "ekpanel-c`+ownerKey(ownerID)+`.slice"`) {
		t.Fatalf("compose:\n%s", imageCompose(ap))
	}
	ap.Owner = "../etc"
	if validateApp(ap) == nil {
		t.Fatal("bad owner accepted")
	}
}
