// Package dnssync decides how to bring a Cloudflare zone to the records ekPanel wants,
// changing only records ekPanel made and only while nobody else changed them.
package dnssync

import (
	"slices"
	"strings"

	"ekpanel/internal/cloudflare"
)

// Want is a record ekPanel wants in the zone. Mail records are DNSOnly: Cloudflare's proxy
// carries only web traffic.
type Want struct {
	Name, Type, Content string
	Priority            int // MX
	DNSOnly             bool
}

// Managed is a record ekPanel made, as it last wrote it.
type Managed struct {
	RecordID, Name, Type, Content string
	Proxied                       bool
}

func (m Managed) key() string { return m.Type + " " + m.Name }
func (w Want) key() string    { return w.Type + " " + w.Name }

// Issue is something a sync does not change by itself.
type Issue struct {
	// Kind is "changed" (a record ekPanel made was changed in Cloudflare), "conflict" (a
	// record ekPanel did not make is in the way) or "no_address" (the server has no public
	// address of that type).
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Want   string `json:"want,omitempty"`
	Have   string `json:"have,omitempty"`
	Server string `json:"server,omitempty"`
}

// Action is one change: to Cloudflare (create, update, delete) and/or to the list of
// managed records (Keep replaces or adds one, Forget removes one).
type Action struct {
	Kind   string // "create", "update", "delete", "adopt", "forget"
	Record cloudflare.Record
	Forget string // record ID to drop from the managed list
}

// Plan compares what is wanted, what ekPanel made and what Cloudflare has. Keys in force
// ("A www.example.com") resolve issues by writing ekPanel's value anyway.
func Plan(want []Want, ours []Managed, have []cloudflare.Record, proxied bool, force map[string]bool) ([]Action, []Issue) {
	var actions []Action
	var issues []Issue
	byID := map[string]cloudflare.Record{}
	for _, r := range have {
		byID[r.ID] = r
	}
	mine := map[string]Managed{}
	for _, m := range ours {
		mine[m.key()] = m
	}
	wanted := map[string]bool{}
	record := func(w Want, p bool) cloudflare.Record {
		r := cloudflare.Record{Type: w.Type, Name: w.Name, Content: w.Content, TTL: 1, Proxied: p && !w.DNSOnly, Comment: cloudflare.ManagedComment}
		if w.Type == "MX" {
			prio := w.Priority
			r.Priority = &prio
		}
		return r
	}
	for _, w := range want {
		wanted[w.key()] = true
		if m, ok := mine[w.key()]; ok {
			cf, exists := byID[m.RecordID]
			switch {
			case !exists:
				// Deleted by hand while the site still needs it: make it again.
				actions = append(actions, Action{Kind: "create", Record: record(w, m.Proxied), Forget: m.RecordID})
			case !sameContent(w.Type, cf.Content, m.Content) || cf.Proxied != m.Proxied:
				if sameContent(w.Type, cf.Content, w.Content) {
					// Changed by hand to what is wanted anyway (or only proxied): keep it so.
					actions = append(actions, Action{Kind: "adopt", Record: cf})
				} else if force[w.key()] {
					r := record(w, cf.Proxied)
					r.ID = cf.ID
					actions = append(actions, Action{Kind: "update", Record: r})
				} else {
					issues = append(issues, Issue{Kind: "changed", Name: w.Name, Type: w.Type, Want: w.Content, Have: cf.Content})
				}
			case !sameContent(w.Type, m.Content, w.Content):
				r := record(w, m.Proxied)
				r.ID = m.RecordID
				actions = append(actions, Action{Kind: "update", Record: r})
			}
			continue
		}
		// Not made by ekPanel yet: look for records already using the name.
		var same, blocking []cloudflare.Record
		for _, r := range have {
			if r.Name != w.Name {
				continue
			}
			switch {
			case r.Type == w.Type && (w.Type != "TXT" || txtKind(r.Content) == txtKind(w.Content)):
				// Other TXT records at a name (site verifications and such) are left alone.
				same = append(same, r)
			case r.Type == "CNAME" || w.Type == "CNAME":
				blocking = append(blocking, r)
			}
		}
		switch {
		case len(blocking) > 0 && force[w.key()]:
			for _, b := range blocking {
				actions = append(actions, Action{Kind: "delete", Record: b})
			}
			actions = append(actions, Action{Kind: "create", Record: record(w, proxied)})
		case len(blocking) > 0:
			issues = append(issues, Issue{Kind: "conflict", Name: w.Name, Type: w.Type, Want: w.Content, Have: blocking[0].Type + " " + blocking[0].Content})
		case slices.ContainsFunc(same, func(r cloudflare.Record) bool { return sameContent(w.Type, r.Content, w.Content) }):
			i := slices.IndexFunc(same, func(r cloudflare.Record) bool { return sameContent(w.Type, r.Content, w.Content) })
			actions = append(actions, Action{Kind: "adopt", Record: same[i]})
		case len(same) > 0 && force[w.key()]:
			r := record(w, same[0].Proxied)
			r.ID = same[0].ID
			actions = append(actions, Action{Kind: "update", Record: r})
		case len(same) > 0:
			issues = append(issues, Issue{Kind: "conflict", Name: w.Name, Type: w.Type, Want: w.Content, Have: same[0].Type + " " + same[0].Content})
		default:
			actions = append(actions, Action{Kind: "create", Record: record(w, proxied)})
		}
	}
	// Records ekPanel made that nothing needs any more go, unless someone changed them.
	for _, m := range ours {
		if wanted[m.key()] {
			continue
		}
		cf, exists := byID[m.RecordID]
		switch {
		case !exists:
			actions = append(actions, Action{Kind: "forget", Forget: m.RecordID})
		case sameContent(m.Type, cf.Content, m.Content) && cf.Proxied == m.Proxied:
			actions = append(actions, Action{Kind: "delete", Record: cf, Forget: m.RecordID})
		default:
			actions = append(actions, Action{Kind: "forget", Forget: m.RecordID})
		}
	}
	return actions, issues
}

// sameContent compares record contents as DNS does: TXT data whether or not it is quoted
// or split into strings, host names without case or a final dot.
func sameContent(typ, a, b string) bool {
	switch typ {
	case "TXT":
		return unquoteTXT(a) == unquoteTXT(b)
	case "MX", "CNAME":
		return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
	}
	return a == b
}

// unquoteTXT turns `"a" "b"` into ab; unquoted text stays as it is.
func unquoteTXT(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	var b strings.Builder
	for s != "" {
		if s[0] != '"' {
			return s
		}
		end := 1
		for end < len(s) && s[end] != '"' {
			if s[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(s) {
			return s
		}
		b.WriteString(strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(s[1:end]))
		s = strings.TrimSpace(s[end+1:])
	}
	return b.String()
}

// txtKind is what a TXT record is for, by its version tag ("v=spf1", "v=DKIM1", …).
func txtKind(content string) string {
	tag, _, _ := strings.Cut(unquoteTXT(content), ";")
	tag, _, _ = strings.Cut(tag, " ")
	return strings.ToLower(tag)
}
