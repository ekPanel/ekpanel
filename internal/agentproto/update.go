package agentproto

import (
	"errors"
	"net/url"

	"ekpanel/internal/release"
)

// OpSystemUpdate installs a release of the panel or the agent on a server. The agent
// downloads it, checks its signature, swaps the binary (keeping the old one as NAME.prev)
// and replies; then it restarts the program, and puts the old binary back if the new one
// does not come up. The panel is only updated on the server that runs it.
const OpSystemUpdate = "system.update"

const (
	ComponentPanel = "panel"
	ComponentAgent = "agent"
)

type SystemUpdateParams struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	// ReleasesURL is where releases are published (GitHub's layout).
	ReleasesURL string `json:"releases_url"`
}

func (p SystemUpdateParams) Validate() error {
	if p.Component != ComponentPanel && p.Component != ComponentAgent {
		return errors.New("component must be panel or agent")
	}
	if !release.Valid(p.Version) {
		return errors.New("not a release version")
	}
	u, err := url.Parse(p.ReleasesURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return errors.New("releases_url must be an http(s) URL")
	}
	return nil
}

type SystemUpdateResult struct {
	Previous string `json:"previous"`
	Version  string `json:"version"`
	// Backup is the panel database dump taken before a panel update.
	Backup string `json:"backup,omitempty"`
}
