// Package cloudflaretest is a small in-memory stand-in for Cloudflare's API, for tests.
package cloudflaretest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"ekpanel/internal/cloudflare"
)

type zone struct {
	cloudflare.Zone
	SSL     string
	Purges  int
	Records []cloudflare.Record
}

// Fake answers like Cloudflare for the tokens in Tokens.
type Fake struct {
	mu      sync.Mutex
	Tokens  map[string]bool
	Account cloudflare.Account
	zones   []*zone
	next    int
}

func New(token string) *Fake {
	return &Fake{Tokens: map[string]bool{token: true}, Account: cloudflare.Account{ID: "acc1", Name: "Test account"}}
}

func (f *Fake) id() string {
	f.next++
	return fmt.Sprintf("%032x", f.next)
}

// AddZone adds an active zone and returns its ID.
func (f *Fake) AddZone(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := &zone{Zone: cloudflare.Zone{ID: f.id(), Name: name, Status: "active", NameServers: []string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"}, Account: f.Account}, SSL: "flexible"}
	f.zones = append(f.zones, z)
	return z.ID
}

// Records returns a copy of a zone's records.
func (f *Fake) Records(zoneID string) []cloudflare.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	if z := f.zone(zoneID); z != nil {
		return slices.Clone(z.Records)
	}
	return nil
}

// SetRecord adds or changes a record by hand, as someone in Cloudflare's dashboard would.
func (f *Fake) SetRecord(zoneID string, r cloudflare.Record) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := f.zone(zoneID)
	for i := range z.Records {
		if z.Records[i].ID == r.ID {
			z.Records[i] = r
			return r.ID
		}
	}
	r.ID = f.id()
	z.Records = append(z.Records, r)
	return r.ID
}

// DeleteRecord removes a record by hand.
func (f *Fake) DeleteRecord(zoneID, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	z := f.zone(zoneID)
	z.Records = slices.DeleteFunc(z.Records, func(r cloudflare.Record) bool { return r.ID == id })
}

// SSL and Purges report a zone's SSL mode and how often its cache was purged.
func (f *Fake) SSL(zoneID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zone(zoneID).SSL
}

func (f *Fake) Purges(zoneID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.zone(zoneID).Purges
}

func (f *Fake) zone(id string) *zone {
	for _, z := range f.zones {
		if z.ID == id {
			return z
		}
	}
	return nil
}

func reply(w http.ResponseWriter, status int, result any, errs ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	messages := []map[string]any{}
	for _, e := range errs {
		messages = append(messages, map[string]any{"code": 1000, "message": e})
	}
	json.NewEncoder(w).Encode(map[string]any{"success": status < 300, "errors": messages, "result": result,
		"result_info": map[string]int{"page": 1, "total_pages": 1}})
}

func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !f.Tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		reply(w, http.StatusUnauthorized, nil, "Invalid API Token")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/client/v4"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "accounts":
		reply(w, 200, []cloudflare.Account{f.Account})
	case len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodGet:
		out := []cloudflare.Zone{}
		for _, z := range f.zones {
			out = append(out, z.Zone)
		}
		reply(w, 200, out)
	case len(parts) == 1 && parts[0] == "zones" && r.Method == http.MethodPost:
		var body struct {
			Name    string `json:"name"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, z := range f.zones {
			if z.Name == body.Name {
				reply(w, 400, nil, "A zone with this name already exists")
				return
			}
		}
		z := &zone{Zone: cloudflare.Zone{ID: f.id(), Name: body.Name, Status: "pending", NameServers: []string{"ada.ns.cloudflare.com", "bob.ns.cloudflare.com"}, Account: f.Account}, SSL: "flexible"}
		f.zones = append(f.zones, z)
		reply(w, 200, z.Zone)
	case len(parts) >= 2 && parts[0] == "zones":
		z := f.zone(parts[1])
		if z == nil {
			reply(w, 404, nil, "Could not find zone")
			return
		}
		f.serveZone(w, r, z, parts[2:])
	default:
		reply(w, 404, nil, "No route")
	}
}

func (f *Fake) serveZone(w http.ResponseWriter, r *http.Request, z *zone, parts []string) {
	switch {
	case len(parts) == 0:
		reply(w, 200, z.Zone)
	case parts[0] == "settings" && len(parts) == 2 && parts[1] == "ssl":
		if r.Method == http.MethodPatch {
			var body struct{ Value string }
			json.NewDecoder(r.Body).Decode(&body)
			z.SSL = body.Value
		}
		reply(w, 200, map[string]string{"id": "ssl", "value": z.SSL})
	case parts[0] == "purge_cache":
		z.Purges++
		reply(w, 200, map[string]string{"id": z.ID})
	case parts[0] == "dns_records" && len(parts) == 1 && r.Method == http.MethodGet:
		reply(w, 200, append([]cloudflare.Record{}, z.Records...)) // [] rather than null, like Cloudflare
	case parts[0] == "dns_records" && len(parts) == 1 && r.Method == http.MethodPost:
		var rec cloudflare.Record
		json.NewDecoder(r.Body).Decode(&rec)
		for _, x := range z.Records {
			if x.Name == rec.Name && (x.Type == "CNAME" || rec.Type == "CNAME") {
				reply(w, 400, nil, "An A, AAAA, or CNAME record with that host already exists.")
				return
			}
		}
		rec.ID, rec.Proxiable = f.id(), rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME"
		if rec.TTL == 0 {
			rec.TTL = 1
		}
		z.Records = append(z.Records, rec)
		reply(w, 200, rec)
	case parts[0] == "dns_records" && len(parts) == 2:
		i := slices.IndexFunc(z.Records, func(x cloudflare.Record) bool { return x.ID == parts[1] })
		if i < 0 {
			reply(w, 404, nil, "Record not found")
			return
		}
		switch r.Method {
		case http.MethodDelete:
			z.Records = slices.Delete(z.Records, i, i+1)
			reply(w, 200, map[string]string{"id": parts[1]})
		case http.MethodPut, http.MethodPatch:
			var rec cloudflare.Record
			json.NewDecoder(r.Body).Decode(&rec)
			rec.ID, rec.Proxiable = parts[1], rec.Type == "A" || rec.Type == "AAAA" || rec.Type == "CNAME"
			if rec.TTL == 0 {
				rec.TTL = 1
			}
			z.Records[i] = rec
			reply(w, 200, rec)
		default:
			reply(w, 200, z.Records[i])
		}
	default:
		reply(w, 404, nil, "No route "+strconv.Itoa(len(parts)))
	}
}
