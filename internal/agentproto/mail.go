package agentproto

import (
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ---- Email ----
//
// A server with email runs Stalwart for SMTP, IMAP and POP3, and Roundcube as webmail at
// the mail host name. The panel sends the whole mail setup in StateApplyParams.Mail; nil
// means email is off, and the agent stops the mail server (keeping its data).

// MailSpec is the email setup of one server.
type MailSpec struct {
	// Hostname is the server's mail host name: the MX target of every domain, the name in
	// the SMTP greeting and the address of webmail and of mail apps' settings.
	Hostname string `json:"hostname"`
	TLS      string `json:"tls"` // "auto" (public CA) or "internal" (self-signed), for webmail and mail
	// Domains are every mail domain of the server. A domain leaves the server only with
	// Deleted; within a domain, its mailboxes and forwarders are the whole list.
	Domains []MailDomainSpec `json:"domains"`
}

type MailDomainSpec struct {
	ID         string          `json:"id"`
	Generation int64           `json:"generation"`
	Domain     string          `json:"domain"`
	CatchAll   string          `json:"catch_all,omitempty"` // mailbox that gets mail for unknown addresses
	Mailboxes  []MailboxSpec   `json:"mailboxes"`
	Forwarders []ForwarderSpec `json:"forwarders"`
	Deleted    bool            `json:"deleted,omitempty"`
}

// MailboxSpec is one mailbox. PasswordHash is SHA-512 crypt; the password itself never
// reaches the agent.
type MailboxSpec struct {
	ID           string `json:"id"`
	Name         string `json:"name"` // local part, before the @
	PasswordHash string `json:"password_hash"`
	QuotaMB      int    `json:"quota_mb,omitempty"` // zero means no limit
	// ForwardTo gets a copy of every message; without KeepCopy the mailbox keeps none.
	ForwardTo []string   `json:"forward_to,omitempty"`
	KeepCopy  bool       `json:"keep_copy"`
	AutoReply *AutoReply `json:"auto_reply,omitempty"`
	// Locked mailboxes cannot sign in (their owner is suspended); mail still arrives.
	Locked bool `json:"locked,omitempty"`
}

// AutoReply answers incoming mail, once a day per sender, between the dates (if any).
type AutoReply struct {
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Start   string `json:"start,omitempty"` // YYYY-MM-DD
	End     string `json:"end,omitempty"`
}

// ForwarderSpec is an address without a mailbox that passes mail on to others.
type ForwarderSpec struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	To   []string `json:"to"`
}

type MailResult struct {
	OK      bool               `json:"ok"`
	Error   string             `json:"error,omitempty"`
	Domains []MailDomainResult `json:"domains"`
}

type MailDomainResult struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	// DNS are the records the domain needs for mail to arrive and be trusted.
	DNS []MailDNSRecord `json:"dns,omitempty"`
	// Usage is the bytes each mailbox holds, by mailbox ID.
	Usage map[string]int64 `json:"usage,omitempty"`
}

// MailDNSRecord is one DNS record of a mail domain, with its name fully qualified and no
// trailing dot. Priority is for MX.
type MailDNSRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority int    `json:"priority,omitempty"`
}

// MailPorts are opened in the firewall on a server with email: SMTP, submission (implicit
// TLS and STARTTLS), IMAPS and POP3S.
var MailPorts = []int{25, 465, 587, 993, 995}

// MailCheck tests what decides whether other mail servers accept this server's mail.
const OpMailCheck = "mail.check"

type MailCheckParams struct {
	Hostname string `json:"hostname"`
}

type MailCheckResult struct {
	Hostname string `json:"hostname"`
	// HostnameAddresses are what the host name resolves to.
	HostnameAddresses []string `json:"hostname_addresses"`
	// Port25 is "open" when this server can reach other mail servers, "blocked" when its
	// provider blocks outgoing mail.
	Port25    string             `json:"port25"`
	Addresses []MailAddressCheck `json:"addresses"`
}

type MailAddressCheck struct {
	Address string   `json:"address"`
	PTR     []string `json:"ptr"`     // reverse DNS names of the address
	PTROK   bool     `json:"ptr_ok"`  // the reverse name is the mail host name
	Listed  []string `json:"listed"`  // blocklists that list the address
	Unknown []string `json:"unknown"` // blocklists that did not answer
	Checked []string `json:"checked"` // every blocklist asked
}

const MaxMailboxQuotaMB = 1 << 20 // 1 TiB

var (
	mailHostRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+([a-z]{2,63}|xn--[a-z0-9-]{1,59})$`)
	localPartRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9._+-]{0,62}[a-z0-9])?$`)
	mailIDRE    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha512RE    = regexp.MustCompile(`^\$6\$(rounds=[0-9]{4,9}\$)?[./A-Za-z0-9]{1,16}\$[./A-Za-z0-9]{86}$`)
)

// ValidSHA512Crypt reports whether h is a SHA-512 crypt hash, as mailboxes keep passwords.
func ValidSHA512Crypt(h string) bool { return sha512RE.MatchString(h) }

// ValidMailHost reports whether h is a valid host or domain name for mail.
func ValidMailHost(h string) bool { return len(h) <= 253 && mailHostRE.MatchString(h) }

// ValidLocalPart reports whether s may come before the @ of an address ekPanel creates.
func ValidLocalPart(s string) bool {
	return localPartRE.MatchString(s) && !strings.Contains(s, "..")
}

// ValidForwardAddress reports whether s is a plain address mail can be forwarded to.
func ValidForwardAddress(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, " \"\\<>\r\n") {
		return false
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	return at > 0 && ValidMailHost(strings.ToLower(s[at+1:]))
}

// MaxAutoReplyBody is the longest auto-reply text.
const MaxAutoReplyBody = 4000

// MaxForwards is how many addresses one mailbox or forwarder may pass mail to.
const MaxForwards = 20

func validDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
}

// Validate checks an auto-reply as both panel and agent require it.
func (r AutoReply) Validate() error {
	switch {
	case strings.TrimSpace(r.Subject) == "" || len(r.Subject) > 200 || strings.ContainsAny(r.Subject, "\r\n"):
		return errors.New("the auto-reply needs a subject of one line, up to 200 characters")
	case strings.TrimSpace(r.Body) == "" || len(r.Body) > MaxAutoReplyBody:
		return fmt.Errorf("the auto-reply needs a message of up to %d characters", MaxAutoReplyBody)
	case r.Start != "" && !validDate(r.Start), r.End != "" && !validDate(r.End):
		return errors.New("auto-reply dates must be YYYY-MM-DD")
	case r.Start != "" && r.End != "" && r.End < r.Start:
		return errors.New("the auto-reply ends before it starts")
	}
	return nil
}

// ValidateForwards checks a list of forwarding addresses.
func ValidateForwards(to []string) error {
	if len(to) > MaxForwards {
		return fmt.Errorf("at most %d addresses", MaxForwards)
	}
	for _, a := range to {
		if !ValidForwardAddress(a) {
			return fmt.Errorf("invalid address %q", a)
		}
	}
	return nil
}

// Validate checks a whole mail setup. The agent refuses anything that does not pass.
func (m MailSpec) Validate() error {
	if !ValidMailHost(m.Hostname) {
		return fmt.Errorf("invalid mail host name %q", m.Hostname)
	}
	if m.TLS != "auto" && m.TLS != "internal" {
		return fmt.Errorf("unknown TLS mode %q", m.TLS)
	}
	seen := map[string]bool{}
	for _, d := range m.Domains {
		if seen[d.Domain] {
			return fmt.Errorf("mail domain %s is listed twice", d.Domain)
		}
		seen[d.Domain] = true
	}
	return nil
}

// Validate checks one mail domain and everything in it.
func (d MailDomainSpec) Validate() error {
	if !mailIDRE.MatchString(d.ID) {
		return errors.New("invalid mail domain id")
	}
	if !ValidMailHost(d.Domain) {
		return fmt.Errorf("invalid mail domain %q", d.Domain)
	}
	if d.Deleted {
		return nil
	}
	names := map[string]bool{}
	use := func(name string) error {
		if !ValidLocalPart(name) {
			return fmt.Errorf("invalid address %q", name+"@"+d.Domain)
		}
		if names[name] {
			return fmt.Errorf("the address %s is used twice", name+"@"+d.Domain)
		}
		names[name] = true
		return nil
	}
	for _, mb := range d.Mailboxes {
		if !mailIDRE.MatchString(mb.ID) {
			return errors.New("invalid mailbox id")
		}
		if err := use(mb.Name); err != nil {
			return err
		}
		if !sha512RE.MatchString(mb.PasswordHash) {
			return fmt.Errorf("invalid password hash for %s", mb.Name)
		}
		if mb.QuotaMB < 0 || mb.QuotaMB > MaxMailboxQuotaMB {
			return fmt.Errorf("invalid quota for %s", mb.Name)
		}
		if err := ValidateForwards(mb.ForwardTo); err != nil {
			return fmt.Errorf("forwarding of %s: %w", mb.Name, err)
		}
		if !mb.KeepCopy && len(mb.ForwardTo) == 0 {
			return fmt.Errorf("%s would keep no mail and forward none", mb.Name)
		}
		if mb.AutoReply != nil {
			if err := mb.AutoReply.Validate(); err != nil {
				return fmt.Errorf("%s: %w", mb.Name, err)
			}
		}
	}
	for _, f := range d.Forwarders {
		if !mailIDRE.MatchString(f.ID) {
			return errors.New("invalid forwarder id")
		}
		if err := use(f.Name); err != nil {
			return err
		}
		if len(f.To) == 0 {
			return fmt.Errorf("the forwarder %s has no addresses", f.Name)
		}
		if err := ValidateForwards(f.To); err != nil {
			return fmt.Errorf("forwarder %s: %w", f.Name, err)
		}
	}
	if d.CatchAll != "" && !slices.ContainsFunc(d.Mailboxes, func(mb MailboxSpec) bool { return mb.Name == d.CatchAll }) {
		return fmt.Errorf("the catch-all mailbox %s does not exist", d.CatchAll)
	}
	return nil
}

// ParseMailZone picks, from the zone file Stalwart writes for a domain, the records mail
// needs: MX, SPF, DKIM, DMARC and TLS reporting. Everything else in it (service records,
// MTA-STS and autoconfig names) needs web endpoints ekPanel does not serve.
func ParseMailZone(domain, zone string) ([]MailDNSRecord, error) {
	var out []MailDNSRecord
	// Join records that continue over several lines in parentheses.
	var entries []string
	var cur strings.Builder
	depth := 0
	for line := range strings.Lines(zone) {
		line = strings.TrimSpace(line)
		if line == "" && depth == 0 {
			continue
		}
		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(line)
		depth += strings.Count(line, "(") - strings.Count(line, ")")
		if depth <= 0 {
			entries = append(entries, cur.String())
			cur.Reset()
			depth = 0
		}
	}
	if depth != 0 {
		return nil, errors.New("unbalanced parentheses in zone")
	}
	for _, e := range entries {
		fields := strings.Fields(strings.NewReplacer("(", " ", ")", " ").Replace(e))
		if len(fields) < 4 || fields[1] != "IN" {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(fields[0], "."))
		if name != domain && !strings.HasSuffix(name, "."+domain) {
			return nil, fmt.Errorf("record %s is outside %s", name, domain)
		}
		typ, rest := fields[2], fields[3:]
		switch typ {
		case "MX":
			if len(rest) != 2 || name != domain {
				continue
			}
			prio, err := strconv.Atoi(rest[0])
			if err != nil || prio < 0 || prio > 65535 {
				return nil, fmt.Errorf("bad MX priority %q", rest[0])
			}
			host := strings.ToLower(strings.TrimSuffix(rest[1], "."))
			if !ValidMailHost(host) {
				return nil, fmt.Errorf("bad MX host %q", rest[1])
			}
			out = append(out, MailDNSRecord{Type: "MX", Name: name, Content: host, Priority: prio})
		case "TXT":
			text, err := joinTXT(strings.TrimSpace(e[strings.Index(e, " TXT ")+5:]))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			keep := (name == domain && strings.HasPrefix(text, "v=spf1 ")) ||
				(strings.HasSuffix(name, "._domainkey."+domain) && strings.HasPrefix(text, "v=DKIM1;")) ||
				(name == "_dmarc."+domain && strings.HasPrefix(text, "v=DMARC1;")) ||
				(name == "_smtp._tls."+domain && strings.HasPrefix(text, "v=TLSRPTv1;"))
			if keep {
				out = append(out, MailDNSRecord{Type: "TXT", Name: name, Content: text})
			}
		}
	}
	return out, nil
}

// joinTXT turns `"a" "b"` (optionally in parentheses) into ab.
func joinTXT(s string) (string, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "("), ")"))
	var b strings.Builder
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] != '"' {
			return "", errors.New("TXT data is not quoted")
		}
		end := 1
		for end < len(s) && s[end] != '"' {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(s) {
			return "", errors.New("unterminated TXT string")
		}
		part := s[1:end]
		part = strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(part)
		b.WriteString(part)
		s = s[end+1:]
	}
	return b.String(), nil
}
