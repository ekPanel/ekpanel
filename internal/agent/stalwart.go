package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// jmapClient talks to Stalwart's management API: JMAP with Stalwart's own objects
// ("x:Domain", "x:Account", …) next to the standard ones (SieveScript).
type jmapClient struct {
	base, user, password string
	http                 *http.Client
}

var jmapUsing = []string{"urn:ietf:params:jmap:core", "urn:stalwart:jmap", "urn:ietf:params:jmap:sieve"}

// jmapError is a method-level error, or an object that could not be created, changed or
// destroyed.
type jmapError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
	Validation  []struct {
		Type     string `json:"type"`
		Property string `json:"property"`
	} `json:"validationErrors"`
	Linked []struct {
		Object string `json:"object"`
	} `json:"linkedObjects"`
}

func (e jmapError) Error() string {
	msg := e.Type
	if e.Description != "" {
		msg += ": " + e.Description
	}
	for _, v := range e.Validation {
		msg += fmt.Sprintf(" (%s %s)", v.Type, v.Property)
	}
	for _, l := range e.Linked {
		msg += " (used by " + l.Object + ")"
	}
	return msg
}

func (c *jmapClient) auth(req *http.Request) {
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.password)))
}

// call runs one method and decodes its arguments into out.
func (c *jmapClient) call(ctx context.Context, method string, args any, out any) error {
	body, err := json.Marshal(map[string]any{"using": jmapUsing, "methodCalls": []any{[]any{method, args, "0"}}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/jmap/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mail server API: %s: %s", resp.Status, strings.TrimSpace(string(raw[:min(len(raw), 300)])))
	}
	var r struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &r); err != nil || len(r.MethodResponses) != 1 || len(r.MethodResponses[0]) != 3 {
		return fmt.Errorf("mail server API: unexpected answer to %s", method)
	}
	var name string
	json.Unmarshal(r.MethodResponses[0][0], &name)
	if name == "error" {
		var e jmapError
		json.Unmarshal(r.MethodResponses[0][1], &e)
		return fmt.Errorf("%s: %w", method, e)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.MethodResponses[0][1], out)
}

// get lists every object of a kind (ids nil) or the given ones.
func (c *jmapClient) get(ctx context.Context, object string, args map[string]any, list any) error {
	if args == nil {
		args = map[string]any{}
	}
	if _, ok := args["ids"]; !ok {
		args["ids"] = nil
	}
	var out struct {
		List json.RawMessage `json:"list"`
	}
	if err := c.call(ctx, object+"/get", args, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.List, list)
}

type setResult struct {
	Created      map[string]struct{ ID string } `json:"created"`
	NotCreated   map[string]jmapError           `json:"notCreated"`
	NotUpdated   map[string]jmapError           `json:"notUpdated"`
	NotDestroyed map[string]jmapError           `json:"notDestroyed"`
}

// set changes objects and fails if any part was refused. It returns the IDs of created
// objects by their creation key.
func (c *jmapClient) set(ctx context.Context, object string, args map[string]any) (map[string]string, error) {
	var out setResult
	if err := c.call(ctx, object+"/set", args, &out); err != nil {
		return nil, err
	}
	var errs []error
	for k, e := range out.NotCreated {
		errs = append(errs, fmt.Errorf("create %s %s: %w", object, k, e))
	}
	for k, e := range out.NotUpdated {
		errs = append(errs, fmt.Errorf("update %s %s: %w", object, k, e))
	}
	for k, e := range out.NotDestroyed {
		errs = append(errs, fmt.Errorf("destroy %s %s: %w", object, k, e))
	}
	ids := map[string]string{}
	for k, v := range out.Created {
		ids[k] = v.ID
	}
	return ids, errors.Join(errs...)
}

func (c *jmapClient) create(ctx context.Context, object string, value any) (string, error) {
	ids, err := c.set(ctx, object, map[string]any{"create": map[string]any{"n": value}})
	return ids["n"], err
}

func (c *jmapClient) update(ctx context.Context, object, id string, patch any) error {
	_, err := c.set(ctx, object, map[string]any{"update": map[string]any{id: patch}})
	return err
}

func (c *jmapClient) destroy(ctx context.Context, object string, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := c.set(ctx, object, map[string]any{"destroy": ids})
	return err
}

// action runs a server action such as reloading settings or certificates.
func (c *jmapClient) action(ctx context.Context, kind string) error {
	_, err := c.create(ctx, "x:Action", map[string]any{"@type": kind})
	return err
}

// upload stores a blob in an account and returns its ID.
func (c *jmapClient) upload(ctx context.Context, accountID, contentType string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/jmap/upload/"+accountID+"/", bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", contentType)
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		BlobID string `json:"blobId"`
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return "", fmt.Errorf("upload: %s: %s", resp.Status, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.BlobID == "" {
		return "", errors.New("upload: no blob ID in the answer")
	}
	return out.BlobID, nil
}

// ready waits until the API answers with these credentials.
func (c *jmapClient) ready(ctx context.Context, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	var err error
	for {
		var out json.RawMessage
		if err = c.call(ctx, "x:Domain/get", map[string]any{"ids": []string{}}, &out); err == nil {
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("the mail server did not start: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- Stalwart objects, as much of them as ekPanel reads ----

type swDomain struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	CatchAll    *string `json:"catchAllAddress"`
	DNSZoneFile string  `json:"dnsZoneFile"`
}

type swAccount struct {
	ID       string           `json:"id"`
	Type     string           `json:"@type"`
	Name     string           `json:"name"`
	DomainID string           `json:"domainId"`
	Quotas   map[string]int64 `json:"quotas"`
	Used     int64            `json:"usedDiskQuota"`
}

type swList struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	DomainID   string          `json:"domainId"`
	Recipients map[string]bool `json:"recipients"`
}

type swListener struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Protocol    string          `json:"protocol"`
	Bind        map[string]bool `json:"bind"`
	ImplicitTLS bool            `json:"implicitTls"`
}

type swSieve struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	IsActive bool   `json:"isActive"`
}

type swDkim struct {
	ID       string `json:"id"`
	DomainID string `json:"domainId"`
}

type swSettings struct {
	DefaultHostname      string  `json:"defaultHostname"`
	DefaultDomainID      string  `json:"defaultDomainId"`
	DefaultCertificateID *string `json:"defaultCertificateId"`
}
