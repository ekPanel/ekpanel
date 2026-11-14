package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"ekpanel/internal/agentproto"
)

var testHash = "$6$saltsalt$" + strings.Repeat("aB3./", 17) + "x"

func mailCpmove() []byte {
	home := []tarFile{
		{name: "public_html/index.html", body: "hi"},
		{name: "etc/shop.com/passwd", body: "info:x:1:1::/home/shop/mail/shop.com/info:/bin/false\nsales:x:1:1::/x:/bin/false\nold:x:1:1::/x:/bin/false\n"},
		{name: "etc/shop.com/shadow", body: "info:" + testHash + ":19000::::::\nsales:$1$md5$oldhash:19000::::::\n"},
		{name: "etc/shop.com/quota", body: "info:1048576000\n"},
		{name: "mail/shop.com/info/cur/1700000000.M1.host:2,S", body: "Subject: read\r\n\r\nseen"},
		{name: "mail/shop.com/info/new/1700000100.M2.host", body: "Subject: unread\r\n\r\nnew"},
		{name: "mail/shop.com/info/cur/1700000200.M3.host:2,ST", body: "Subject: deleted\r\n\r\ngone"},
		{name: "mail/shop.com/info/.Sent/cur/1700000300.M4.host:2,S", body: "Subject: sent\r\n\r\nout"},
		{name: "mail/shop.com/info/.Work.R&AOk-sum&AOk-/cur/1700000400.M5.host:2,SF", body: "Subject: nested\r\n\r\nwork"},
		{name: "mail/shop.com/info/maildirsize", body: "1000S"},
		{name: "mail/shop.com/sales/cur/1700000500.M6.host:2,", body: "Subject: sales\r\n\r\nx"},
		{name: "mail/shop.com/old/storage/m.1", body: "mdbox"},
		{name: "mail/cur/1700000600.M7.host:2,", body: "Subject: account's own\r\n\r\nx"},
	}
	files := []tarFile{
		{name: "cpmove-shop/cp/shop", body: "USER=shop\nDNS=shop.com\nIP=203.0.113.5\n"},
		{name: "cpmove-shop/userdata/main", body: "---\nmain_domain: shop.com\n"},
		{name: "cpmove-shop/userdata/shop.com", body: "documentroot: /home/shop/public_html\n"},
		{name: "cpmove-shop/va/shop.com", body: "*: info@shop.com\nhello@shop.com: info@shop.com, boss@example.org\nbot@shop.com: |/home/shop/bot.php\ninfo@shop.com: copy@example.org\n"},
		{name: "cpmove-shop/dnszones/shop.com.db", body: "$TTL 14400\nshop.com. IN A 203.0.113.5\nwww IN CNAME shop.com.\n"},
		{name: "cpmove-shop/homedir.tar", body: string(tarOf(home))},
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(tarOf(files))
	zw.Close()
	return gz.Bytes()
}

// fakeJMAP is the mail server's API with one domain and its accounts.
type fakeJMAP struct {
	mu       sync.Mutex
	folders  map[string][]jmapMailbox // by account
	imported []map[string]any         // Email/import entries, with "account"
	blobs    map[string]string
}

func newFakeJMAP(t *testing.T) *jmapClient {
	f := &fakeJMAP{folders: map[string][]jmapMailbox{}, blobs: map[string]string{}}
	role := func(r string) *string { return &r }
	for _, a := range []string{"a1", "a2"} {
		f.folders[a] = []jmapMailbox{{ID: a + "-in", Name: "Inbox", Role: role("inbox")}, {ID: a + "-sent", Name: "Sent Items", Role: role("sent")}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if acct, ok := strings.CutPrefix(r.URL.Path, "/jmap/upload/"); ok {
			b, _ := io.ReadAll(r.Body)
			id := "blob" + string(rune('A'+len(f.blobs)))
			f.blobs[id] = strings.TrimSuffix(acct, "/") + ":" + string(b)
			json.NewEncoder(w).Encode(map[string]string{"blobId": id})
			return
		}
		var req struct {
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var method string
		json.Unmarshal(req.MethodCalls[0][0], &method)
		var args map[string]any
		json.Unmarshal(req.MethodCalls[0][1], &args)
		var out any
		switch method {
		case "x:Domain/get":
			out = map[string]any{"list": []map[string]string{{"id": "d1", "name": "shop.com"}}}
		case "x:Account/get":
			out = map[string]any{"list": []map[string]string{{"id": "a1", "name": "info", "domainId": "d1"}, {"id": "a2", "name": "sales", "domainId": "d1"}}}
		case "Mailbox/get":
			out = map[string]any{"list": f.folders[args["accountId"].(string)]}
		case "Mailbox/set":
			acct := args["accountId"].(string)
			v := args["create"].(map[string]any)["f"].(map[string]any)
			b := jmapMailbox{ID: acct + "-" + v["name"].(string), Name: v["name"].(string)}
			if p, ok := v["parentId"].(string); ok {
				b.ParentID = &p
			}
			f.folders[acct] = append(f.folders[acct], b)
			out = map[string]any{"created": map[string]any{"f": map[string]string{"id": b.ID}}}
		case "Email/import":
			created := map[string]any{}
			for k, e := range args["emails"].(map[string]any) {
				m := e.(map[string]any)
				m["account"] = args["accountId"]
				m["body"] = f.blobs[m["blobId"].(string)]
				f.imported = append(f.imported, m)
				created[k] = map[string]string{"id": "e" + k}
			}
			out = map[string]any{"created": created}
		default:
			t.Errorf("unexpected %s", method)
		}
		json.NewEncoder(w).Encode(map[string]any{"methodResponses": [][]any{{method, out, "0"}}})
	}))
	t.Cleanup(srv.Close)
	fakeJMAPState = f
	return &jmapClient{base: srv.URL, user: "ekpanel", password: "x", http: srv.Client()}
}

var fakeJMAPState *fakeJMAP

func TestImportCPanelMail(t *testing.T) {
	var loaded []string
	s := transferSystem(t, &loaded)
	os.MkdirAll(s.path(agentproto.ImportDir), 0o700)
	os.WriteFile(s.path(agentproto.ImportDir+"/cpmove-shop.tar.gz"), mailCpmove(), 0o600)

	out, opErr := importInspect(context.Background(), s, agentproto.ImportInspectParams{File: "cpmove-shop.tar.gz"})
	if opErr != nil {
		t.Fatal(opErr)
	}
	plan := out.(agentproto.CPanelPlan)
	if plan.OldIP != "203.0.113.5" || len(plan.Zones) != 1 || plan.Zones[0].Name != "shop.com" || len(plan.Zones[0].Records) != 2 {
		t.Fatalf("ip %q zones %+v", plan.OldIP, plan.Zones)
	}
	if len(plan.Mail) != 1 {
		t.Fatalf("mail %+v", plan.Mail)
	}
	d := plan.Mail[0]
	names := func() (n []string) {
		for _, m := range d.Mailboxes {
			n = append(n, m.Name)
		}
		return
	}()
	if d.Domain != "shop.com" || !slices.Equal(names, []string{"info", "old", "sales"}) || d.CatchAll != "info" {
		t.Fatalf("domain %+v", d)
	}
	info, old, sales := d.Mailboxes[0], d.Mailboxes[1], d.Mailboxes[2]
	if info.PasswordHash != testHash || info.QuotaMB != 1000 || info.Messages != 5 || sales.PasswordHash != "" || sales.Messages != 1 || !old.Mdbox {
		t.Fatalf("mailboxes %+v", d.Mailboxes)
	}
	if len(d.Forwarders) != 2 || d.Forwarders[0].Name != "hello" || !slices.Equal(d.Forwarders[0].To, []string{"info@shop.com", "boss@example.org"}) ||
		d.Forwarders[1].Name != "info" || len(d.Other) != 1 || !strings.Contains(d.Other[0], "bot.php") {
		t.Fatalf("forwarders %+v other %q", d.Forwarders, d.Other)
	}

	c := newFakeJMAP(t)
	importMailClient = func(*system) (*jmapClient, error) { return c, nil }
	t.Cleanup(func() { importMailClient = mailClient })
	os.MkdirAll(s.path("/home/shop_1/public_html"), 0o750)
	res, opErr := importApply(context.Background(), s, agentproto.ImportApplyParams{File: "cpmove-shop.tar.gz",
		Sites: []agentproto.ImportSite{{DocRoot: "public_html", User: "shop_1"}}, MainUser: "shop_1",
		Mail: []agentproto.ImportMailbox{{Domain: "shop.com", Name: "info"}}})
	if opErr != nil {
		t.Fatal(opErr)
	}
	r := res.(agentproto.ImportApplyResult)
	if r.MailMessages != 4 || r.MailFailed != 0 {
		t.Fatalf("messages %d failed %d", r.MailMessages, r.MailFailed)
	}
	f := fakeJMAPState
	got := map[string]string{} // subject → folder and keywords
	for _, m := range f.imported {
		if m["account"] != "a1" {
			t.Fatalf("a message went to %v", m["account"])
		}
		body := m["body"].(string)
		subject := body[strings.Index(body, "Subject: ")+9 : strings.Index(body, "\r\n")]
		var folder string
		for id := range m["mailboxIds"].(map[string]any) {
			folder = id
		}
		var kws []string
		for k := range m["keywords"].(map[string]any) {
			kws = append(kws, k)
		}
		slices.Sort(kws)
		got[subject] = folder + " " + strings.Join(kws, ",") + " " + m["receivedAt"].(string)
	}
	want := map[string]string{
		"read":   "a1-in $seen 2023-11-14T22:13:20Z",
		"unread": "a1-in  2023-11-14T22:15:00Z",
		"sent":   "a1-sent $seen 2023-11-14T22:18:20Z",
		"nested": "a1-Résumé $flagged,$seen 2023-11-14T22:20:00Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("imported %v", got)
	}
	// Work was made, and Résumé inside it.
	var work, resume *jmapMailbox
	for i, b := range f.folders["a1"] {
		switch b.Name {
		case "Work":
			work = &f.folders["a1"][i]
		case "Résumé":
			resume = &f.folders["a1"][i]
		}
	}
	if work == nil || resume == nil || resume.ParentID == nil || *resume.ParentID != work.ID {
		t.Fatalf("folders %+v", f.folders["a1"])
	}
	// Mail of mailboxes not chosen stays out, and is not put into the website.
	if _, err := os.Stat(s.path("/home/shop_1/mail")); err == nil {
		t.Fatal("mail was copied into the home folder")
	}
}

func TestDecodeIMAPUTF7(t *testing.T) {
	for in, want := range map[string]string{"Sent": "Sent", "R&AOk-sum&AOk-": "Résumé", "&-amp": "&amp", "&ZeVnLIqe-": "日本語"} {
		if got := decodeIMAPUTF7(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}
