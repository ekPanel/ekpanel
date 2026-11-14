package agentproto

import (
	"strconv"
	"strings"
)

// ParseZoneFile reads a zone in the master file format (RFC 1035, as BIND and cPanel write
// it) into records with absolute names, no trailing dot. SOA records are left out; unread
// counts records of other types, and lines that could not be read.
func ParseZoneFile(text, zone string) (records []HostedRecord, unread int) {
	origin := strings.ToLower(strings.TrimSuffix(zone, "."))
	ttl := 14400
	owner := origin
	for _, line := range zoneLines(text) {
		if len(line.fields) == 0 {
			continue
		}
		f := line.fields
		switch strings.ToUpper(f[0]) {
		case "$ORIGIN":
			if len(f) > 1 {
				origin = absName(f[1], origin)
			}
			continue
		case "$TTL":
			if len(f) > 1 {
				if t, ok := zoneTTL(f[1]); ok {
					ttl = t
				}
			}
			continue
		}
		if strings.HasPrefix(f[0], "$") {
			unread++ // $INCLUDE, $GENERATE
			continue
		}
		if !line.indented {
			owner = absName(f[0], origin)
			f = f[1:]
		}
		recTTL := ttl
		// TTL and class, in either order.
		for range 2 {
			if len(f) == 0 {
				break
			}
			if t, ok := zoneTTL(f[0]); ok {
				recTTL, f = t, f[1:]
			} else if c := strings.ToUpper(f[0]); c == "IN" || c == "CH" || c == "HS" {
				f = f[1:]
			}
		}
		if len(f) == 0 {
			unread++
			continue
		}
		typ, data := strings.ToUpper(f[0]), f[1:]
		rec := HostedRecord{Name: owner, Type: typ, TTL: min(max(recTTL, MinTTL), MaxTTL)}
		ok := true
		switch typ {
		case "SOA":
			continue
		case "A", "AAAA":
			ok = len(data) == 1
			if ok {
				rec.Content = data[0]
			}
		case "CNAME", "NS":
			ok = len(data) == 1
			if ok {
				rec.Content = absName(data[0], origin)
			}
		case "MX":
			p, err := strconv.Atoi(firstOf(data))
			ok = len(data) == 2 && err == nil
			if ok {
				rec.Priority, rec.Content = p, absName(data[1], origin)
			}
		case "SRV":
			p, err := strconv.Atoi(firstOf(data))
			ok = len(data) == 4 && err == nil
			if ok {
				target := absName(data[3], origin)
				if data[3] == "." {
					target = ""
				}
				rec.Priority, rec.Content = p, data[1]+" "+data[2]+" "+target
			}
		case "TXT", "SPF":
			rec.Type = "TXT"
			ok = len(data) > 0
			var b strings.Builder
			for i, d := range data {
				// Quoted strings join as they are; bare words with a space between.
				if i > 0 && !line.quoted[d] {
					b.WriteByte(' ')
				}
				b.WriteString(d)
			}
			rec.Content = b.String()
		case "CAA":
			ok = len(data) == 3
			if ok {
				rec.Content = data[0] + " " + data[1] + ` "` + data[2] + `"`
			}
		default:
			ok = false
		}
		if !ok {
			unread++
			continue
		}
		records = append(records, rec)
	}
	return records, unread
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// absName makes a name absolute in origin: "@" is the origin, "www" is www.origin and
// "host.example.org." stays itself.
func absName(name, origin string) string {
	name = strings.ToLower(name)
	switch {
	case name == "@":
		return origin
	case strings.HasSuffix(name, "."):
		return strings.TrimSuffix(name, ".")
	}
	return name + "." + origin
}

// zoneTTL reads a TTL: seconds, or with units such as 1h30m.
func zoneTTL(s string) (int, bool) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	total, num := 0, 0
	seen := false
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= '0' && c <= '9':
			num, seen = num*10+int(c-'0'), true
		case seen && strings.ContainsRune("smhdw", c):
			total += num * map[rune]int{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}[c]
			num, seen = 0, false
		default:
			return 0, false
		}
	}
	return total + num, !seen || num >= 0
}

type zoneLine struct {
	fields   []string
	quoted   map[string]bool // fields that were quoted strings
	indented bool            // no owner name: the previous one
}

// zoneLines splits the file into logical lines (parentheses join physical ones), with
// comments removed and quoted strings unquoted.
func zoneLines(text string) []zoneLine {
	var out []zoneLine
	cur := zoneLine{quoted: map[string]bool{}}
	depth := 0
	started := false
	var field strings.Builder
	inField, inQuote := false, false
	flush := func(quoted bool) {
		if inField || quoted {
			s := field.String()
			cur.fields = append(cur.fields, s)
			if quoted {
				cur.quoted[s] = true
			}
		}
		field.Reset()
		inField = false
	}
	end := func() {
		flush(false)
		if len(cur.fields) > 0 {
			out = append(out, cur)
		}
		cur = zoneLine{quoted: map[string]bool{}}
		started = false
	}
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if !started && c != '\n' {
			started = true
			cur.indented = c == ' ' || c == '\t'
		}
		switch {
		case inQuote:
			switch c {
			case '\\':
				if i+3 < len(rs) && isDigits(rs[i+1:i+4]) {
					n, _ := strconv.Atoi(string(rs[i+1 : i+4]))
					field.WriteRune(rune(n))
					i += 3
				} else if i+1 < len(rs) {
					i++
					field.WriteRune(rs[i])
				}
			case '"':
				inQuote = false
				flush(true)
			default:
				field.WriteRune(c)
			}
		case c == '"':
			flush(false)
			inQuote = true
		case c == ';':
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i--
		case c == '(':
			flush(false)
			depth++
		case c == ')':
			flush(false)
			if depth > 0 {
				depth--
			}
		case c == '\n':
			if depth > 0 {
				flush(false)
				continue
			}
			end()
		case c == ' ' || c == '\t' || c == '\r':
			flush(false)
		default:
			field.WriteRune(c)
			inField = true
		}
	}
	end()
	return out
}

func isDigits(rs []rune) bool {
	for _, r := range rs {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
