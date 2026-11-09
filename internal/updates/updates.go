// Package updates looks for new ekPanel releases and tells the administrators about them.
package updates

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ekpanel/internal/release"
	"ekpanel/internal/store"
)

// Interval is how often the panel looks for a new release.
const Interval = 6 * time.Hour

const settingKey = "update.latest"

// Latest is what the last check found.
type Latest struct {
	Version     string    `json:"version,omitempty"`
	PublishedAt time.Time `json:"published_at,omitzero"`
	NotesURL    string    `json:"notes_url,omitempty"`
	CheckedAt   time.Time `json:"checked_at,omitzero"`
	// Error is why the last check failed ("" when it worked).
	Error string `json:"error,omitempty"`
	// Notified is the newest version administrators were told about.
	Notified string `json:"notified,omitempty"`
}

type Checker struct {
	store   *store.Store
	client  *release.Client
	current string
	log     *slog.Logger
	// Notify tells the administrators about a new version.
	Notify func(ctx context.Context, version, notesURL string)

	mu sync.Mutex // one check at a time
}

func New(st *store.Store, client *release.Client, current string, log *slog.Logger) *Checker {
	return &Checker{store: st, client: client, current: current, log: log}
}

// Get returns the result of the last check.
func (c *Checker) Get(ctx context.Context) (Latest, error) {
	var l Latest
	_, err := c.store.GetSetting(ctx, settingKey, &l)
	return l, err
}

// Check asks for the latest release now.
func (c *Checker) Check(ctx context.Context) (Latest, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l, err := c.Get(ctx)
	if err != nil {
		return l, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := c.client.Latest(ctx)
	l.CheckedAt = time.Now()
	if err != nil {
		l.Error = err.Error()
	} else {
		l.Version, l.PublishedAt, l.NotesURL, l.Error = info.Version, info.PublishedAt, info.NotesURL, ""
		if release.Valid(c.current) && release.Compare(info.Version, c.current) > 0 && release.Compare(info.Version, l.Notified) > 0 {
			if c.Notify != nil {
				c.Notify(ctx, info.Version, info.NotesURL)
			}
			l.Notified = info.Version
		}
	}
	if err := c.store.SetSetting(context.WithoutCancel(ctx), settingKey, l); err != nil {
		return l, err
	}
	return l, nil
}

// Run checks a minute after the panel starts and then every Interval, until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	wait := time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = Interval
		if l, err := c.Check(ctx); err != nil {
			c.log.Warn("check for ekPanel updates", "err", err)
		} else if l.Error != "" {
			c.log.Warn("check for ekPanel updates", "err", l.Error)
		}
	}
}
