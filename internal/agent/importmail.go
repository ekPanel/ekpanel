package agent

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"ekpanel/internal/agentproto"
)

// Email and DNS zones of a cPanel backup.
//
// In the home folder, etc/<domain>/passwd lists a domain's mailboxes, etc/<domain>/shadow
// their password hashes and etc/<domain>/quota their quotas; mail/<domain>/<name>/ is a
// mailbox's Maildir (folders as .Name subfolders, Maildir++). Outside it, va/<domain> holds
// the forwarders and dnszones/<zone>.db the zones.

// ---- Inspect ----

// mailScan collects what the home folder says about email while the archive is read.
type mailScan struct {
	etc   map[string]string                    // etc/<domain>/{passwd,shadow,quota}
	boxes map[string]*agentproto.CPanelMailbox // by "<domain>/<name>"
}

func newMailScan() *mailScan {
	return &mailScan{etc: map[string]string{}, boxes: map[string]*agentproto.CPanelMailbox{}}
}

func (m *mailScan) box(domain, name string) *agentproto.CPanelMailbox {
	k := domain + "/" + name
	if m.boxes[k] == nil {
		m.boxes[k] = &agentproto.CPanelMailbox{Name: name}
	}
	return m.boxes[k]
}

// home looks at one entry of the home folder.
func (m *mailScan) home(rel string, h *tar.Header, body io.Reader) {
	parts := strings.Split(rel, "/")
	if len(parts) == 3 && parts[0] == "etc" && h.Typeflag == tar.TypeReg && h.Size < 4<<20 &&
		(parts[2] == "passwd" || parts[2] == "shadow" || parts[2] == "quota") {
		b, _ := io.ReadAll(io.LimitReader(body, 4<<20))
		m.etc[rel] = string(b)
		return
	}
	domain, name, rest, ok := mailPath(rel)
	if !ok {
		return
	}
	b := m.box(domain, name)
	if len(rest) > 0 && rest[0] == "storage" {
		b.Mdbox = true
	}
	if _, _, ok := maildirMessage(rest); ok && h.Typeflag == tar.TypeReg {
		b.Messages++
		b.Bytes += h.Size
	}
}

// mailPath splits mail/<domain>/<name>/rest…; the account's own mail (mail/cur …) is not
// a domain's mailbox.
func mailPath(rel string) (domain, name string, rest []string, ok bool) {
	parts := strings.Split(rel, "/")
	if len(parts) < 4 || parts[0] != "mail" || !strings.Contains(parts[1], ".") || strings.HasPrefix(parts[1], ".") ||
		!agentproto.ValidMailHost(strings.ToLower(parts[1])) {
		return "", "", nil, false
	}
	return strings.ToLower(parts[1]), strings.ToLower(parts[2]), parts[3:], true
}

// maildirMessage reads [".Folder",] "cur"|"new", file: the folder ("" for the inbox),
// and whether the message is new (not yet seen by a mail program).
func maildirMessage(rest []string) (folder string, isNew, ok bool) {
	if len(rest) == 3 && strings.HasPrefix(rest[0], ".") && len(rest[0]) > 1 {
		folder, rest = rest[0][1:], rest[1:]
	}
	if len(rest) != 2 || (rest[0] != "cur" && rest[0] != "new") || rest[1] == "" {
		return "", false, false
	}
	return folder, rest[0] == "new", true
}

// plan puts the domains together, from etc/ and va/.
func (m *mailScan) plan(small map[string]string) []agentproto.CPanelMailDomain {
	domains := map[string]*agentproto.CPanelMailDomain{}
	get := func(d string) *agentproto.CPanelMailDomain {
		d = strings.ToLower(d)
		if domains[d] == nil {
			domains[d] = &agentproto.CPanelMailDomain{Domain: d, Mailboxes: []agentproto.CPanelMailbox{}, Forwarders: []agentproto.CPanelForwarder{}}
		}
		return domains[d]
	}
	field := func(text string) map[string][]string {
		out := map[string][]string{}
		for _, l := range strings.Split(text, "\n") {
			f := strings.Split(strings.TrimSpace(l), ":")
			if len(f) >= 2 && f[0] != "" {
				out[strings.ToLower(f[0])] = f[1:]
			}
		}
		return out
	}
	for k, v := range m.etc {
		parts := strings.Split(k, "/")
		if parts[2] != "passwd" || !agentproto.ValidMailHost(strings.ToLower(parts[1])) {
			continue
		}
		d := get(parts[1])
		shadow := field(m.etc["etc/"+parts[1]+"/shadow"])
		quota := field(m.etc["etc/"+parts[1]+"/quota"])
		for name := range field(v) {
			mb := agentproto.CPanelMailbox{Name: name}
			if b := m.boxes[d.Domain+"/"+name]; b != nil {
				mb = *b
			}
			if s := shadow[name]; len(s) > 0 && agentproto.ValidSHA512Crypt(s[0]) {
				mb.PasswordHash = s[0]
			}
			if q := quota[name]; len(q) > 0 {
				if n, err := strconv.ParseInt(strings.TrimSpace(q[0]), 10, 64); err == nil && n > 0 {
					mb.QuotaMB = int(min((n+(1<<20)-1)>>20, math.MaxInt32))
				}
			}
			d.Mailboxes = append(d.Mailboxes, mb)
		}
	}
	for k, v := range small {
		name, ok := strings.CutPrefix(k, "va/")
		if !ok || !agentproto.ValidMailHost(strings.ToLower(name)) {
			continue
		}
		d := get(name)
		for _, line := range strings.Split(v, "\n") {
			line = strings.TrimSpace(line)
			addr, dests, ok := strings.Cut(line, ":")
			if !ok || line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			addr = strings.ToLower(strings.TrimSpace(addr))
			local := strings.TrimSuffix(addr, "@"+d.Domain)
			var to []string
			other := false
			for _, dest := range strings.Split(dests, ",") {
				dest = strings.Trim(strings.TrimSpace(dest), `"`)
				switch {
				case dest == "":
				case strings.Contains(dest, "@") && !strings.ContainsAny(dest, "|/: "):
					to = append(to, strings.ToLower(dest))
				default:
					other = true // a program, a file, :fail:, :blackhole: or the account itself
				}
			}
			if local == "*" {
				// Catch-all: to one of the domain's own mailboxes, or nothing to import.
				if len(to) == 1 && strings.HasSuffix(to[0], "@"+d.Domain) {
					d.CatchAll = strings.TrimSuffix(to[0], "@"+d.Domain)
				} else if !strings.Contains(dests, ":fail:") && !strings.Contains(dests, ":blackhole:") && strings.TrimSpace(dests) != "" {
					if !isAccountName(strings.TrimSpace(dests)) {
						d.Other = append(d.Other, line)
					}
				}
				continue
			}
			if strings.Contains(local, "@") {
				continue // another domain's address
			}
			if other {
				d.Other = append(d.Other, line)
			}
			if len(to) > 0 {
				d.Forwarders = append(d.Forwarders, agentproto.CPanelForwarder{Name: local, To: to})
			}
		}
	}
	out := make([]agentproto.CPanelMailDomain, 0, len(domains))
	for _, d := range domains {
		if len(d.Mailboxes) == 0 && len(d.Forwarders) == 0 && d.CatchAll == "" && len(d.Other) == 0 {
			continue
		}
		sort.Slice(d.Mailboxes, func(i, j int) bool { return d.Mailboxes[i].Name < d.Mailboxes[j].Name })
		sort.Slice(d.Forwarders, func(i, j int) bool { return d.Forwarders[i].Name < d.Forwarders[j].Name })
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// isAccountName: cPanel's default address sends unknown mail to the account itself.
func isAccountName(s string) bool { return agentproto.ValidCPanelUser(s) }

func zonesFrom(small map[string]string) []agentproto.CPanelZone {
	out := []agentproto.CPanelZone{}
	for k, v := range small {
		name, ok := strings.CutPrefix(k, "dnszones/")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSuffix(name, ".db"))
		if !agentproto.ValidZoneName(name) {
			continue
		}
		recs, unread := agentproto.ParseZoneFile(v, name)
		if recs == nil {
			recs = []agentproto.HostedRecord{}
		}
		out = append(out, agentproto.CPanelZone{Name: name, Records: recs, Unread: unread})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- Apply: messages into the mail server ----

// importMailClient reaches the mail server's API (tests replace it).
var importMailClient = mailClient

// maxImportMessage: larger messages are not imported (no mail server takes them anyway).
const maxImportMessage = 100 << 20

// mailImporter puts Maildir messages into mail server accounts, through JMAP as the
// administrator: blobs are uploaded one by one and imported in batches.
type mailImporter struct {
	c        *jmapClient
	accounts map[string]string            // address → account ID
	folders  map[string]map[string]string // account ID → folder path ("" inbox) → mailbox ID
	pending  map[string]map[string]any    // account ID → Email/import entries by key
	res      *agentproto.ImportApplyResult
	seq      int
}

func newMailImporter(ctx context.Context, s *system, res *agentproto.ImportApplyResult) (*mailImporter, error) {
	c, err := importMailClient(s)
	if err != nil {
		return nil, err
	}
	var domains []swDomain
	if err := c.get(ctx, "x:Domain", map[string]any{"properties": []string{"name"}}, &domains); err != nil {
		return nil, fmt.Errorf("the mail server: %w", err)
	}
	var accounts []swAccount
	if err := c.get(ctx, "x:Account", map[string]any{"properties": []string{"name", "domainId"}}, &accounts); err != nil {
		return nil, fmt.Errorf("the mail server: %w", err)
	}
	byID := map[string]string{}
	for _, d := range domains {
		byID[d.ID] = d.Name
	}
	m := &mailImporter{c: c, accounts: map[string]string{}, folders: map[string]map[string]string{}, pending: map[string]map[string]any{}, res: res}
	for _, a := range accounts {
		if d := byID[a.DomainID]; d != "" {
			m.accounts[a.Name+"@"+d] = a.ID
		}
	}
	return m, nil
}

type jmapMailbox struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Role     *string `json:"role"`
	ParentID *string `json:"parentId"`
}

// standardFolders are Maildir++ folder names of mail programs and the role they have.
var standardFolders = map[string]string{
	"sent": "sent", "sent items": "sent", "sent messages": "sent", "sent mail": "sent",
	"drafts": "drafts", "trash": "trash", "deleted items": "trash", "deleted messages": "trash",
	"junk": "junk", "spam": "junk", "junk e-mail": "junk", "junk mail": "junk", "archive": "archive",
}

// folder returns the mailbox for a Maildir++ folder ("" is the inbox; "Work.2024" is 2024
// inside Work), making the folders that are missing.
func (m *mailImporter) folder(ctx context.Context, account, maildirName string) (string, error) {
	known := m.folders[account]
	if known == nil {
		var list []jmapMailbox
		if err := m.c.get(ctx, "Mailbox", map[string]any{"accountId": account, "properties": []string{"name", "role", "parentId"}}, &list); err != nil {
			return "", err
		}
		known = map[string]string{}
		byID := map[string]jmapMailbox{}
		for _, b := range list {
			byID[b.ID] = b
		}
		var pathOf func(b jmapMailbox) string
		pathOf = func(b jmapMailbox) string {
			if b.ParentID != nil {
				if p, ok := byID[*b.ParentID]; ok {
					return pathOf(p) + "/" + b.Name
				}
			}
			return b.Name
		}
		for _, b := range list {
			known[strings.ToLower(pathOf(b))] = b.ID
			if b.Role != nil && *b.Role != "" {
				known["role:"+*b.Role] = b.ID
			}
		}
		m.folders[account] = known
	}
	if maildirName == "" {
		if id := known["role:inbox"]; id != "" {
			return id, nil
		}
		return "", fmt.Errorf("the account has no inbox")
	}
	names := strings.Split(decodeIMAPUTF7(maildirName), ".")
	if len(names) == 1 {
		if role := standardFolders[strings.ToLower(names[0])]; role != "" {
			if id := known["role:"+role]; id != "" {
				return id, nil
			}
		}
	}
	parent := ""
	for i, name := range names {
		key := strings.ToLower(strings.Join(names[:i+1], "/"))
		if id := known[key]; id != "" {
			parent = id
			continue
		}
		value := map[string]any{"name": name, "parentId": nil}
		if parent != "" {
			value["parentId"] = parent
		}
		ids, err := m.c.set(ctx, "Mailbox", map[string]any{"accountId": account, "create": map[string]any{"f": value}})
		if err != nil {
			return "", err
		}
		parent = ids["f"]
		known[key] = parent
	}
	return parent, nil
}

// add imports one message of mail/<domain>/<name>/<rest…>.
func (m *mailImporter) add(ctx context.Context, address string, rest []string, h *tar.Header, r io.Reader) error {
	folderName, isNew, ok := maildirMessage(rest)
	if !ok || h.Typeflag != tar.TypeReg {
		return nil
	}
	account := m.accounts[address]
	if account == "" {
		m.res.MailFailed++
		return nil
	}
	file := rest[len(rest)-1]
	keywords := map[string]bool{}
	if _, flags, ok := strings.Cut(file, ":2,"); ok {
		if strings.Contains(flags, "T") {
			return nil // deleted in the mail program, waiting to be expunged
		}
		for f, kw := range map[string]string{"S": "$seen", "R": "$answered", "F": "$flagged", "D": "$draft"} {
			if strings.Contains(flags, f) {
				keywords[kw] = true
			}
		}
	}
	if isNew {
		delete(keywords, "$seen")
	}
	if h.Size > maxImportMessage || h.Size == 0 {
		m.res.MailFailed++
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r, maxImportMessage))
	if err != nil {
		return err
	}
	mailbox, err := m.folder(ctx, account, folderName)
	if err != nil {
		return fmt.Errorf("mail folder %q of %s: %w", folderName, address, err)
	}
	blob, err := m.c.upload(ctx, account, "message/rfc822", body)
	if err != nil {
		m.res.MailFailed++
		return nil
	}
	received := h.ModTime
	// Maildir file names start with the time the message arrived.
	if sec, _, ok := strings.Cut(file, "."); ok {
		if n, err := strconv.ParseInt(sec, 10, 64); err == nil && n > 0 {
			received = time.Unix(n, 0)
		}
	}
	m.seq++
	if m.pending[account] == nil {
		m.pending[account] = map[string]any{}
	}
	m.pending[account][strconv.Itoa(m.seq)] = map[string]any{"blobId": blob, "mailboxIds": map[string]bool{mailbox: true},
		"keywords": keywords, "receivedAt": received.UTC().Format(time.RFC3339)}
	if len(m.pending[account]) >= 50 {
		return m.flush(ctx, account)
	}
	return nil
}

func (m *mailImporter) flush(ctx context.Context, account string) error {
	emails := m.pending[account]
	if len(emails) == 0 {
		return nil
	}
	delete(m.pending, account)
	var out struct {
		Created    map[string]json.RawMessage `json:"created"`
		NotCreated map[string]json.RawMessage `json:"notCreated"`
	}
	if err := m.c.call(ctx, "Email/import", map[string]any{"accountId": account, "emails": emails}, &out); err != nil {
		return fmt.Errorf("put mail into the mailbox: %w", err)
	}
	m.res.MailMessages += len(out.Created)
	m.res.MailFailed += len(out.NotCreated)
	return nil
}

func (m *mailImporter) finish(ctx context.Context) error {
	for _, account := range slices.Sorted(func(yield func(string) bool) {
		for a := range m.pending {
			if !yield(a) {
				return
			}
		}
	}) {
		if err := m.flush(ctx, account); err != nil {
			return err
		}
	}
	return nil
}

// decodeIMAPUTF7 decodes folder names in IMAP's modified UTF-7 ("&AOk-t&AOk-" is "été").
func decodeIMAPUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '&' {
			b.WriteByte(s[i])
			continue
		}
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		enc := s[i+1 : i+j]
		i += j
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := modifiedBase64(enc)
		if err != nil || len(raw)%2 != 0 {
			b.WriteString("&" + enc + "-")
			continue
		}
		u := make([]uint16, len(raw)/2)
		for k := range u {
			u[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		b.WriteString(string(utf16.Decode(u)))
	}
	return b.String()
}

func modifiedBase64(s string) ([]byte, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,"
	var out []byte
	bits, n := 0, 0
	for _, c := range []byte(s) {
		v := strings.IndexByte(alphabet, c)
		if v < 0 {
			return nil, fmt.Errorf("bad character")
		}
		bits = bits<<6 | v
		n += 6
		if n >= 8 {
			n -= 8
			out = append(out, byte(bits>>n))
			bits &= 1<<n - 1
		}
	}
	return out, nil
}
