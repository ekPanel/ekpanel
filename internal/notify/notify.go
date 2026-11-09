// Package notify tells people about events: a server going offline, a failed backup, a
// full disk. Each notification goes to the user's inbox in the panel and to the channels
// they set up (email, Telegram, Discord, Slack or a webhook), in their own language.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ekpanel/internal/secretbox"
	"ekpanel/internal/store"
)

//go:embed locales/*.json
var localeFS embed.FS

type catalog struct {
	Events map[string]struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"events"`
	Open string `json:"open"`
}

var catalogs = map[string]catalog{}

func init() {
	for _, l := range []string{"en", "bn"} {
		b, err := localeFS.ReadFile("locales/" + l + ".json")
		if err != nil {
			panic(err)
		}
		var c catalog
		if err := json.Unmarshal(b, &c); err != nil {
			panic(fmt.Sprintf("notify locale %s: %v", l, err))
		}
		catalogs[l] = c
	}
}

// Events a user can choose for a channel. Administrators get every one; customers get
// those about their own websites, apps, databases, disk space and account.
var (
	AdminEvents    = []string{"server_offline", "server_online", "server_disk", "backup_failed", "backup_partial", "site_error", "certificate_expiring", "app_error", "deploy_failed", "database_error", "mail_error", "wordpress_update", "malware_found", "quota_warning", "quota_full", "task_failed", "login_new_ip", "recovery_code_used", "update_available", "update_finished", "update_failed"}
	CustomerEvents = []string{"site_error", "certificate_expiring", "app_error", "deploy_failed", "database_error", "mail_error", "wordpress_update", "malware_found", "quota_warning", "quota_full", "task_failed", "login_new_ip", "recovery_code_used"}
)

func fill(text string, params map[string]string) string {
	for k, v := range params {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	return text
}

// Message is a notification written out in one language.
type Message struct {
	Event    string            `json:"event"`
	Severity string            `json:"severity"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Link     string            `json:"link,omitempty"`
	Params   map[string]string `json:"params"`
	At       time.Time         `json:"at"`
}

// Render writes a notification in a language, falling back to English.
func Render(locale string, n store.Notification) Message {
	c, ok := catalogs[locale]
	if !ok {
		c = catalogs["en"]
	}
	e, ok := c.Events[n.Event]
	if !ok {
		e = catalogs["en"].Events[n.Event]
	}
	return Message{Event: n.Event, Severity: n.Severity, Title: fill(e.Title, n.Params), Body: fill(e.Body, n.Params),
		Link: n.Link, Params: n.Params, At: n.CreatedAt}
}

func (m Message) text(locale string) string {
	t := m.Title + "\n\n" + m.Body
	if m.Link != "" {
		c, ok := catalogs[locale]
		if !ok {
			c = catalogs["en"]
		}
		t += "\n\n" + fill(c.Open, map[string]string{"link": m.Link})
	}
	return t
}

// ---- Settings ----

// SMTP is the mail server used for email notifications. Password holds the sealed
// password, base64 in JSON.
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password []byte `json:"password,omitempty"`
	From     string `json:"from"`
	TLS      string `json:"tls"` // "starttls", "tls" (port 465) or "none" (local relays only)
}

type Settings struct {
	PanelURL string `json:"panel_url"` // for links in messages, such as https://panel.example.com
	SMTP     SMTP   `json:"smtp"`
}

const settingsKey = "notifications"

// ---- Notifier ----

type Notifier struct {
	store *store.Store
	box   *secretbox.Box
	log   *slog.Logger
	// AllowPrivate lets every channel reach private and local addresses. Off, only
	// administrators' channels can; tests turn it on.
	AllowPrivate bool
}

func New(st *store.Store, box *secretbox.Box, log *slog.Logger) *Notifier {
	return &Notifier{store: st, box: box, log: log}
}

func (n *Notifier) Settings(ctx context.Context) (Settings, error) {
	var s Settings
	_, err := n.store.GetSetting(ctx, settingsKey, &s)
	return s, err
}

func (n *Notifier) SaveSettings(ctx context.Context, s Settings) error {
	return n.store.SetSetting(ctx, settingsKey, s)
}

func (n *Notifier) Seal(b []byte) []byte { return n.box.Seal(b) }

// Event sends a notification to each user listed. path is a panel page such as
// "/sites/<id>"; it becomes a link when the panel's address is known.
func (n *Notifier) Event(ctx context.Context, users []string, event, severity string, params map[string]string, path string) {
	link := ""
	if s, err := n.Settings(ctx); err == nil && s.PanelURL != "" && path != "" {
		link = strings.TrimRight(s.PanelURL, "/") + path
	}
	seen := map[string]bool{}
	for _, u := range users {
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if _, err := n.store.AddNotification(ctx, store.Notification{UserID: u, Event: event, Severity: severity, Params: params, Link: link}); err != nil {
			n.log.Error("add notification", "event", event, "user", u, "err", err)
		}
	}
}

// Admins sends an event to every administrator, and also to owner if given.
func (n *Notifier) Admins(ctx context.Context, owner *string, event, severity string, params map[string]string, path string) {
	users, err := n.store.AdminIDs(ctx)
	if err != nil {
		n.log.Error("list administrators", "err", err)
	}
	if owner != nil {
		users = append(users, *owner)
	}
	n.Event(ctx, users, event, severity, params, path)
}

// Run sends queued notifications to their channels until ctx ends. A failed send is tried
// again after 1, 5 and 30 minutes, then given up.
func (n *Notifier) Run(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	retries := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		due, err := n.store.DueDeliveries(ctx, 20, 2*time.Minute)
		if err != nil {
			if ctx.Err() == nil {
				n.log.Error("load notifications to send", "err", err)
			}
			continue
		}
		for _, d := range due {
			sendErr := n.Send(ctx, d.Channel, Render(d.Locale, d.Notification), d.Locale)
			retryAt := time.Time{}
			if sendErr != nil && d.Attempts <= len(retries) {
				retryAt = time.Now().Add(retries[d.Attempts-1])
			}
			n.store.FinishDelivery(ctx, d.Notification.ID, d.Channel.ID, sendErr, retryAt)
			n.store.ChannelResult(ctx, d.Channel.ID, sendErr)
		}
		n.store.PruneNotifications(ctx, 90*24*time.Hour)
	}
}

// ---- Sending ----

// ErrPrivateAddress means a customer's channel pointed at a local or private address.
var ErrPrivateAddress = errors.New("this address is on a private network")

func privateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) ||
		// Carrier-grade NAT and the 0.0.0.0/8 block
		(ip.To4() != nil && (ip.To4()[0] == 0 || (ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64)))
}

// httpClient refuses connections to private addresses unless allowed. The check runs on the
// address actually dialled, so a name that resolves (or redirects) to one is caught too.
func httpClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); !allowPrivate && (ip == nil || privateIP(ip)) {
			return ErrPrivateAddress
		}
		return nil
	}}
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second, Proxy: nil},
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// ChannelSecret is the secret part of a channel, sealed in the database.
type ChannelSecret struct {
	Token string `json:"token,omitempty"` // Telegram bot token
	URL   string `json:"url,omitempty"`   // Discord, Slack or webhook address
	Key   string `json:"key,omitempty"`   // webhook signing key
}

func (n *Notifier) secret(c store.Channel) (ChannelSecret, error) {
	var s ChannelSecret
	if len(c.Secret) == 0 {
		return s, nil
	}
	plain, err := n.box.Open(c.Secret)
	if err != nil {
		return s, errors.New("cannot read the channel's secret; enter it again")
	}
	return s, json.Unmarshal(plain, &s)
}

// Send delivers one message to one channel now.
func (n *Notifier) Send(ctx context.Context, c store.Channel, m Message, locale string) error {
	sec, err := n.secret(c)
	if err != nil {
		return err
	}
	owner, err := n.store.UserByID(ctx, c.UserID)
	if err != nil {
		return err
	}
	client := httpClient(n.AllowPrivate || owner.Role == "admin")
	text := m.text(locale)
	switch c.Kind {
	case "email":
		return n.sendEmail(ctx, c.Config["address"], m.Title, text)
	case "telegram":
		return postJSON(ctx, client, "https://api.telegram.org/bot"+sec.Token+"/sendMessage",
			map[string]any{"chat_id": c.Config["chat_id"], "text": text, "disable_web_page_preview": true}, nil)
	case "discord":
		return postJSON(ctx, client, sec.URL, map[string]any{"content": truncate(text, 1900)}, nil)
	case "slack":
		return postJSON(ctx, client, sec.URL, map[string]any{"text": text}, nil)
	case "webhook":
		body, _ := json.Marshal(m)
		mac := hmac.New(sha256.New, []byte(sec.Key))
		mac.Write(body)
		return postRaw(ctx, client, sec.URL, body, map[string]string{
			"X-Ekpanel-Event": m.Event, "X-Ekpanel-Signature": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		})
	}
	return fmt.Errorf("unknown channel kind %q", c.Kind)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func postJSON(ctx context.Context, client *http.Client, u string, v any, headers map[string]string) error {
	body, _ := json.Marshal(v)
	return postRaw(ctx, client, u, body, headers)
}

func postRaw(ctx context.Context, client *http.Client, u string, body []byte, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ekPanel")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL holds a bot token or a webhook secret; never repeat it.
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("the service answered %s: %s", resp.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

// sendEmail sends a plain-text message through the panel's SMTP server.
func (n *Notifier) sendEmail(ctx context.Context, to, subject, text string) error {
	s, err := n.Settings(ctx)
	if err != nil {
		return err
	}
	cfg := s.SMTP
	if cfg.Host == "" || cfg.From == "" {
		return errors.New("no mail server is set up; an administrator can add one under notification settings")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	password := ""
	if len(cfg.Password) > 0 {
		p, err := n.box.Open(cfg.Password)
		if err != nil {
			return errors.New("cannot read the mail server's password; enter it again")
		}
		password = string(p)
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	var conn net.Conn
	d := &net.Dialer{Timeout: 15 * time.Second}
	if cfg.TLS == "tls" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: cfg.Host})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	c, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.TLS == "starttls" || cfg.TLS == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS")
		}
		if err := c.StartTLS(&tls.Config{ServerName: cfg.Host}); err != nil {
			return err
		}
	}
	if cfg.Username != "" {
		// PlainAuth refuses to send the password over an unencrypted connection to a remote host.
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, password, cfg.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(cfg.From); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	msg := "From: " + cfg.From + "\r\nTo: " + to + "\r\nSubject: " + mime.QEncoding.Encode("utf-8", subject) +
		"\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: 8bit\r\nAuto-Submitted: auto-generated\r\n\r\n" + strings.ReplaceAll(text, "\n", "\r\n") + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
