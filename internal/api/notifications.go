package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ekpanel/internal/notify"
	"ekpanel/internal/store"
)

var (
	telegramTokenRE = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{30,64}$`)
	telegramChatRE  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
)

const maxChannels = 20

func eventsFor(u *store.User) []string {
	if u.Role == "admin" {
		return notify.AdminEvents
	}
	return notify.CustomerEvents
}

// ---- Inbox ----

type notificationView struct {
	ID        int64      `json:"id"`
	Event     string     `json:"event"`
	Severity  string     `json:"severity"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at"`
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	list, err := s.store.ListNotifications(r.Context(), u.ID, int(queryInt(r, "limit")))
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	unread, err := s.store.UnreadNotifications(r.Context(), u.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]notificationView, 0, len(list))
	for _, n := range list {
		m := notify.Render(u.Locale, n)
		out = append(out, notificationView{ID: n.ID, Event: n.Event, Severity: n.Severity, Title: m.Title, Body: m.Body, Link: n.Link,
			CreatedAt: n.CreatedAt, ReadAt: n.ReadAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": out, "unread": unread})
}

func (s *Server) handleReadNotifications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"` // empty: all
	}
	if !readJSON(w, r, &req) {
		return
	}
	var ids []int64
	if len(req.IDs) > 0 {
		ids = req.IDs
	}
	if err := s.store.MarkNotificationsRead(r.Context(), currentUser(r).ID, ids); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Channels ----

type channelRequest struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Events  []string `json:"events"`
	Enabled *bool    `json:"enabled"`
	Address string   `json:"address"` // email
	ChatID  string   `json:"chat_id"` // telegram
	Token   string   `json:"token"`   // telegram; empty on update keeps it
	URL     string   `json:"url"`     // discord, slack, webhook; empty on update keeps it
	Key     string   `json:"key"`     // webhook signing key; empty makes one
}

// channelView is a channel without its secrets.
type channelView struct {
	store.Channel
	// The webhook signing key, only when it was just made.
	Key string `json:"key,omitempty"`
}

func validHookURL(raw, kind string, admin bool) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || len(raw) > 2000 {
		return "", false
	}
	switch kind {
	case "discord":
		return u.String(), u.Scheme == "https" && (u.Host == "discord.com" || u.Host == "discordapp.com") && strings.HasPrefix(u.Path, "/api/webhooks/")
	case "slack":
		return u.String(), u.Scheme == "https" && u.Host == "hooks.slack.com" && strings.HasPrefix(u.Path, "/services/")
	default:
		// Administrators may post to plain HTTP, for a service on their own network.
		return u.String(), u.Scheme == "https" || (admin && u.Scheme == "http")
	}
}

// channelFrom validates a request into a channel. existing is nil when creating.
func (s *Server) channelFrom(w http.ResponseWriter, r *http.Request, req channelRequest, existing *store.Channel) (store.Channel, string, bool) {
	fail := func(code, msg string) (store.Channel, string, bool) {
		writeError(w, http.StatusBadRequest, code, msg)
		return store.Channel{}, "", false
	}
	u := currentUser(r)
	c := store.Channel{UserID: u.ID, Kind: req.Kind, Name: strings.TrimSpace(req.Name), Enabled: req.Enabled == nil || *req.Enabled, Config: map[string]string{}}
	if existing != nil {
		c.ID, c.Kind, c.UserID = existing.ID, existing.Kind, existing.UserID
	}
	if c.Name == "" {
		c.Name = c.Kind
	}
	if utf8.RuneCountInString(c.Name) > 60 {
		return fail("invalid_name", "enter a name of up to 60 characters")
	}
	allowed := eventsFor(u)
	for _, e := range req.Events {
		if !slices.Contains(allowed, e) {
			return fail("invalid_events", "choose from the listed events")
		}
	}
	c.Events = slices.Compact(slices.Sorted(slices.Values(req.Events)))
	sec := notify.ChannelSecret{}
	newSecret, newKey := false, ""
	switch c.Kind {
	case "email":
		a, err := mail.ParseAddress(strings.TrimSpace(req.Address))
		if err != nil || a.Address != strings.TrimSpace(req.Address) {
			return fail("invalid_email", "enter a valid email address")
		}
		c.Config["address"] = a.Address
	case "telegram":
		chat := strings.TrimSpace(req.ChatID)
		if !telegramChatRE.MatchString(chat) {
			return fail("invalid_chat_id", "enter the chat's numeric ID or its @name")
		}
		c.Config["chat_id"] = chat
		if req.Token != "" || existing == nil {
			if !telegramTokenRE.MatchString(strings.TrimSpace(req.Token)) {
				return fail("invalid_bot_token", "enter the token that @BotFather gave, such as 123456:ABC…")
			}
			sec.Token, newSecret = strings.TrimSpace(req.Token), true
		}
	case "discord", "slack", "webhook":
		var old notify.ChannelSecret
		if existing != nil {
			old, _ = s.channelSecret(*existing)
		}
		sec.URL, sec.Key = old.URL, old.Key
		c.Config["host"] = ""
		if existing != nil {
			c.Config["host"] = existing.Config["host"]
		}
		if req.URL != "" || existing == nil {
			clean, ok := validHookURL(req.URL, c.Kind, u.Role == "admin")
			if !ok {
				return fail("invalid_hook_url", "enter the webhook address the service gave you")
			}
			parsed, _ := url.Parse(clean)
			sec.URL, c.Config["host"], newSecret = clean, parsed.Host, true
		}
		if c.Kind == "webhook" {
			switch {
			case req.Key != "":
				if len(req.Key) < 16 || len(req.Key) > 200 {
					return fail("invalid_hook_key", "the signing key needs 16 to 200 characters")
				}
				sec.Key, newSecret = req.Key, true
			case sec.Key == "":
				b := make([]byte, 24)
				rand.Read(b)
				sec.Key, newKey, newSecret = hex.EncodeToString(b), hex.EncodeToString(b), true
			}
		}
	default:
		return fail("invalid_channel_kind", "choose email, Telegram, Discord, Slack or a webhook")
	}
	if newSecret {
		b, _ := json.Marshal(sec)
		c.Secret = s.opts.Notify.Seal(b)
	}
	return c, newKey, true
}

func (s *Server) channelSecret(c store.Channel) (notify.ChannelSecret, error) {
	var sec notify.ChannelSecret
	plain, err := s.opts.Secrets.Open(c.Secret)
	if err != nil {
		return sec, err
	}
	return sec, json.Unmarshal(plain, &sec)
}

func (s *Server) getChannel(r *http.Request) (store.Channel, error) {
	c, err := s.store.GetChannel(r.Context(), urlID(r))
	if err == nil && c.UserID != currentUser(r).ID {
		return store.Channel{}, store.ErrNotFound
	}
	return c, err
}

func (s *Server) writeChannelError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found", "no such channel")
		return
	}
	s.internalError(w, r, err)
}

func (s *Server) handleListChannels(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListChannels(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"channels": list, "events": eventsFor(currentUser(r))})
}

func (s *Server) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if !readJSON(w, r, &req) {
		return
	}
	list, err := s.store.ListChannels(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if len(list) >= maxChannels {
		writeError(w, http.StatusConflict, "too_many_channels", "you can have up to 20 channels")
		return
	}
	c, key, ok := s.channelFrom(w, r, req, nil)
	if !ok {
		return
	}
	c, err = s.store.SaveChannel(r.Context(), c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "notifications.channel_added", c.Name, map[string]any{"channel_id": c.ID, "kind": c.Kind})
	writeJSON(w, http.StatusCreated, channelView{Channel: c, Key: key})
}

func (s *Server) handleUpdateChannel(w http.ResponseWriter, r *http.Request) {
	var req channelRequest
	if !readJSON(w, r, &req) {
		return
	}
	existing, err := s.getChannel(r)
	if err != nil {
		s.writeChannelError(w, r, err)
		return
	}
	c, key, ok := s.channelFrom(w, r, req, &existing)
	if !ok {
		return
	}
	c, err = s.store.SaveChannel(r.Context(), c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, channelView{Channel: c, Key: key})
}

func (s *Server) handleDeleteChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.getChannel(r)
	if err == nil {
		err = s.store.DeleteChannel(r.Context(), c.ID)
	}
	if err != nil {
		s.writeChannelError(w, r, err)
		return
	}
	s.audit(r, "notifications.channel_removed", c.Name, map[string]any{"channel_id": c.ID})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestChannel sends a test message now and reports what the service answered.
func (s *Server) handleTestChannel(w http.ResponseWriter, r *http.Request) {
	c, err := s.getChannel(r)
	if err != nil {
		s.writeChannelError(w, r, err)
		return
	}
	u := currentUser(r)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	brand := "ekPanel"
	if b, err := s.store.EffectiveBrand(r.Context(), &u.ID, ""); err == nil {
		brand = b.Name
	}
	m := notify.Render(u.Locale, store.Notification{Event: "test", Severity: "info", Params: map[string]string{"brand": brand}, CreatedAt: time.Now()})
	sendErr := s.opts.Notify.Send(ctx, c, m, u.Locale)
	s.store.ChannelResult(r.Context(), c.ID, sendErr)
	if sendErr != nil {
		writeError(w, http.StatusBadGateway, "channel_failed", sendErr.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Settings (administrators) ----

type notifySettingsView struct {
	PanelURL string `json:"panel_url"`
	SMTP     struct {
		Host        string `json:"host"`
		Port        int    `json:"port"`
		Username    string `json:"username"`
		From        string `json:"from"`
		TLS         string `json:"tls"`
		Password    string `json:"password,omitempty"` // write-only; empty keeps the stored one
		PasswordSet bool   `json:"password_set"`
	} `json:"smtp"`
}

func (s *Server) handleGetNotifySettings(w http.ResponseWriter, r *http.Request) {
	set, err := s.opts.Notify.Settings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var v notifySettingsView
	v.PanelURL = set.PanelURL
	v.SMTP.Host, v.SMTP.Port, v.SMTP.Username, v.SMTP.From, v.SMTP.TLS = set.SMTP.Host, set.SMTP.Port, set.SMTP.Username, set.SMTP.From, set.SMTP.TLS
	v.SMTP.PasswordSet = len(set.SMTP.Password) > 0
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetNotifySettings(w http.ResponseWriter, r *http.Request) {
	var v notifySettingsView
	if !readJSON(w, r, &v) {
		return
	}
	old, err := s.opts.Notify.Settings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	set := notify.Settings{PanelURL: strings.TrimRight(strings.TrimSpace(v.PanelURL), "/")}
	if set.PanelURL != "" {
		if u, err := url.Parse(set.PanelURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.RawQuery != "" {
			writeError(w, http.StatusBadRequest, "invalid_panel_url", "enter the address people use to open the panel, such as https://panel.example.com")
			return
		}
	}
	sm := v.SMTP
	sm.Host = strings.TrimSpace(sm.Host)
	if sm.Host != "" {
		from, err := mail.ParseAddress(sm.From)
		switch {
		case len(sm.Host) > 253 || strings.ContainsAny(sm.Host, " /:@"):
			writeError(w, http.StatusBadRequest, "invalid_smtp", "enter the mail server's host name")
			return
		case sm.Port < 1 || sm.Port > 65535:
			writeError(w, http.StatusBadRequest, "invalid_smtp", "enter the mail server's port, such as 587")
			return
		case sm.TLS != "starttls" && sm.TLS != "tls" && sm.TLS != "none":
			writeError(w, http.StatusBadRequest, "invalid_smtp", "choose how to secure the connection")
			return
		case err != nil || strings.ContainsAny(sm.From, "\r\n"):
			writeError(w, http.StatusBadRequest, "invalid_email", "enter a valid sender address")
			return
		default:
			_ = from
		}
		set.SMTP = notify.SMTP{Host: sm.Host, Port: sm.Port, Username: strings.TrimSpace(sm.Username), From: sm.From, TLS: sm.TLS, Password: old.SMTP.Password}
		if sm.Password != "" {
			set.SMTP.Password = s.opts.Notify.Seal([]byte(sm.Password))
		}
		if set.SMTP.Username == "" {
			set.SMTP.Password = nil
		}
	}
	if err := s.opts.Notify.SaveSettings(r.Context(), set); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.audit(r, "notifications.settings_changed", "", map[string]any{"smtp_host": set.SMTP.Host, "panel_url": set.PanelURL})
	s.handleGetNotifySettings(w, r)
}

// checkNewSignIn tells a user when they sign in from an address not seen in 90 days. It
// must run before this sign-in is written to the audit log.
// notifyRecoveryCodeUsed tells the account a recovery code signed it in, and how many are left.
func (s *Server) notifyRecoveryCodeUsed(r *http.Request, u *store.User, left int) {
	if s.opts.Notify == nil {
		return
	}
	s.opts.Notify.Event(context.WithoutCancel(r.Context()), []string{u.ID}, "recovery_code_used", "warning",
		map[string]string{"ip": clientIP(r), "left": strconv.Itoa(left)}, "/settings/security")
}

func (s *Server) checkNewSignIn(r *http.Request, u *store.User) {
	if s.opts.Notify == nil {
		return
	}
	ip := clientIP(r)
	seen, any, err := s.store.LoggedInFrom(r.Context(), u.ID, ip)
	if err != nil || seen || !any {
		return // the very first sign-in is expected
	}
	s.opts.Notify.Event(context.WithoutCancel(r.Context()), []string{u.ID}, "login_new_ip", "warning", map[string]string{"ip": ip}, "/settings/security")
}
