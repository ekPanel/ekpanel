// Package cloudflare is the part of Cloudflare's API ekPanel uses: zones, DNS records, the
// SSL mode and cache purges, with an API token.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is Cloudflare's API. EKPANEL_CLOUDFLARE_API replaces it, for tests.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// ManagedComment marks the records ekPanel creates, also for people looking in Cloudflare.
const ManagedComment = "Managed by ekPanel"

// BaseURL is where the client sends requests.
func BaseURL() string {
	if u := os.Getenv("EKPANEL_CLOUDFLARE_API"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultBaseURL
}

type Client struct {
	Token string
	Base  string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, Base: BaseURL(), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Error is an answer of Cloudflare's API that was not a success.
type Error struct {
	Status   int
	Messages []string
}

func (e *Error) Error() string {
	if len(e.Messages) == 0 {
		return fmt.Sprintf("Cloudflare answered %d", e.Status)
	}
	return "Cloudflare: " + strings.Join(e.Messages, "; ")
}

// IsAuth reports whether err means the token is not valid or lacks a permission.
func IsAuth(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiMessage    `json:"errors"`
	Result  json.RawMessage `json:"result"`
	Info    *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"result_info"`
}

type apiMessage struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) (*envelope, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.Base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Cloudflare: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, &Error{Status: resp.StatusCode, Messages: []string{"unexpected answer"}}
	}
	if resp.StatusCode >= 300 || !env.Success {
		e := &Error{Status: resp.StatusCode}
		for _, m := range env.Errors {
			e.Messages = append(e.Messages, m.Message)
		}
		return nil, e
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return nil, err
		}
	}
	return &env, nil
}

// pages gets every page of a list.
func pages[T any](ctx context.Context, c *Client, path string, query url.Values, perPage int) ([]T, error) {
	var all []T
	for page := 1; ; page++ {
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("page", fmt.Sprint(page))
		q.Set("per_page", fmt.Sprint(perPage))
		var batch []T
		env, err := c.do(ctx, http.MethodGet, path, q, nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if env.Info == nil || page >= env.Info.TotalPages || len(batch) == 0 {
			return all, nil
		}
	}
}

// Verify checks that the token works, by listing a zone.
func (c *Client) Verify(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/zones", url.Values{"per_page": {"5"}}, nil, nil)
	return err
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Accounts lists the accounts the token can see (it may not see any).
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	return pages[Account](ctx, c, "/accounts", nil, 50)
}

type Zone struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"` // "active", "pending" (nameservers not set yet) …
	NameServers []string `json:"name_servers"`
	Account     Account  `json:"account"`
}

func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	return pages[Zone](ctx, c, "/zones", nil, 50)
}

func (c *Client) Zone(ctx context.Context, id string) (Zone, error) {
	var z Zone
	_, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(id), nil, nil, &z)
	return z, err
}

// CreateZone adds a domain to the account; it stays pending until the domain's
// nameservers are changed to the ones returned.
func (c *Client) CreateZone(ctx context.Context, accountID, name string) (Zone, error) {
	var z Zone
	_, err := c.do(ctx, http.MethodPost, "/zones", nil, map[string]any{"name": name, "account": map[string]string{"id": accountID}, "type": "full"}, &z)
	return z, err
}

type Record struct {
	ID        string `json:"id,omitempty"`
	Type      string `json:"type"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	TTL       int    `json:"ttl"` // 1 means automatic
	Proxied   bool   `json:"proxied"`
	Proxiable bool   `json:"proxiable,omitempty"`
	Priority  *int   `json:"priority,omitempty"` // MX
	Comment   string `json:"comment,omitempty"`
}

func (c *Client) Records(ctx context.Context, zoneID string) ([]Record, error) {
	return pages[Record](ctx, c, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, 5000)
}

func (c *Client) CreateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out Record
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records", nil, r, &out)
	return out, err
}

func (c *Client) UpdateRecord(ctx context.Context, zoneID string, r Record) (Record, error) {
	var out Record
	_, err := c.do(ctx, http.MethodPut, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(r.ID), nil, r, &out)
	return out, err
}

func (c *Client) DeleteRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.do(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil, nil, nil)
	return err
}

// SSLMode is the zone's SSL/TLS encryption mode: "off", "flexible", "full" or "strict".
func (c *Client) SSLMode(ctx context.Context, zoneID string) (string, error) {
	var s struct {
		Value string `json:"value"`
	}
	_, err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/settings/ssl", nil, nil, &s)
	return s.Value, err
}

func (c *Client) SetSSLMode(ctx context.Context, zoneID, mode string) error {
	_, err := c.do(ctx, http.MethodPatch, "/zones/"+url.PathEscape(zoneID)+"/settings/ssl", nil, map[string]string{"value": mode}, nil)
	return err
}

// PurgeCache empties the zone's cache on Cloudflare.
func (c *Client) PurgeCache(ctx context.Context, zoneID string) error {
	_, err := c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/purge_cache", nil, map[string]bool{"purge_everything": true}, nil)
	return err
}
