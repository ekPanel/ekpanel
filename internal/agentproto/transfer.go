package agentproto

import (
	"errors"
	"regexp"
	"strings"
)

// Moving things between servers (see PLAN.md, 4c). The panel opens an export stream on the
// source agent (OpTransferExport) and an import stream on the target (OpTransferImport) and
// passes the data between them. Flow control: the source sends a data chunk only while it
// has credit, which the panel grants with TypeStreamCredit envelopes as the target confirms
// (also with TypeStreamCredit) each chunk it has written. When the source is done it closes
// its stream with reason "done" and a TransferResult; the panel then closes the target's
// input (TypeStreamClose, reason "eof"), and the target answers with its own close.
const (
	OpTransferExport = "transfer.export"
	OpTransferImport = "transfer.import"
	// OpTransferFinish puts an imported site's files and databases in place (after the
	// target was reconciled); OpTransferDiscard removes an unused import.
	OpTransferFinish  = "transfer.finish"
	OpTransferDiscard = "transfer.discard"

	TypeStreamCredit = "stream_credit"

	// TransferWindow is how many chunks may be on their way at once.
	TransferWindow = 256
	// IncomingDir holds imports on the target until they are used.
	IncomingDir = "/srv/ekpanel/incoming"
)

// TransferDatabase is one database to carry, by engine and name.
type TransferDatabase struct {
	Engine string `json:"engine"`
	Name   string `json:"name"`
	// User owns the database's objects on the target (OpTransferFinish only).
	User string `json:"user,omitempty"`
}

// TransferExportParams: what to send. Domains are the site's domain and aliases, whose
// certificates are taken from the web server's storage.
type TransferExportParams struct {
	Stream    string             `json:"stream"`
	User      string             `json:"user,omitempty"`
	Domains   []string           `json:"domains,omitempty"`
	Databases []TransferDatabase `json:"databases,omitempty"`
	// App is an app to move: it is stopped, then its Docker volumes are sent; Image (a Git
	// app's current build) goes along when set.
	App   string `json:"app,omitempty"`
	Image string `json:"image,omitempty"`
}

type TransferImportParams struct {
	Stream string `json:"stream"`
	Move   string `json:"move"`
}

type TransferFinishParams struct {
	Move      string             `json:"move"`
	User      string             `json:"user,omitempty"`
	Domain    string             `json:"domain,omitempty"`
	Databases []TransferDatabase `json:"databases,omitempty"`
	// App gets the copied volumes (it was set up by the panel first); it is started again
	// unless Stopped.
	App     string `json:"app,omitempty"`
	Stopped bool   `json:"stopped,omitempty"`
}

type TransferDiscardParams struct {
	Move string `json:"move"`
}

// TransferResult ends an export or import stream.
type TransferResult struct {
	Bytes     int64 `json:"bytes"`
	Files     bool  `json:"files"`
	Databases int   `json:"databases"`
	Certs     int   `json:"certs"`
	Volumes   int   `json:"volumes"`
	Image     bool  `json:"image,omitempty"`
}

type TransferFinishResult struct {
	// Status is what the site's home page answered on the target ("" when not checked).
	Status int `json:"status,omitempty"`
}

var (
	uuidPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	linuxUserRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	dbNameRE        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	appNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)
	gitImagePattern = regexp.MustCompile(`^ekpanel/[a-z][a-z0-9-]{0,30}[a-z0-9]:[a-z0-9]{1,32}$`)
	transferHost    = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

func validTransferDBs(dbs []TransferDatabase, withUser bool) error {
	for _, d := range dbs {
		if (d.Engine != "mariadb" && d.Engine != "postgresql") || !dbNameRE.MatchString(d.Name) {
			return errors.New("invalid database")
		}
		if withUser && !dbNameRE.MatchString(d.User) {
			return errors.New("invalid database user")
		}
	}
	return nil
}

func (p TransferExportParams) Validate() error {
	if p.Stream == "" {
		return errors.New("no stream")
	}
	if p.User != "" && !linuxUserRE.MatchString(p.User) {
		return errors.New("invalid user")
	}
	for _, d := range p.Domains {
		if !transferHost.MatchString(d) {
			return errors.New("invalid domain")
		}
	}
	if p.App != "" && !appNamePattern.MatchString(p.App) {
		return errors.New("invalid app")
	}
	if p.Image != "" && (p.App == "" || !gitImagePattern.MatchString(p.Image) || !strings.HasPrefix(p.Image, GitImageRepo(p.App)+":")) {
		return errors.New("only an app's own build can go along")
	}
	return validTransferDBs(p.Databases, false)
}

func (p TransferImportParams) Validate() error {
	if p.Stream == "" || !uuidPattern.MatchString(p.Move) {
		return errors.New("invalid move")
	}
	return nil
}

func (p TransferFinishParams) Validate() error {
	if !uuidPattern.MatchString(p.Move) {
		return errors.New("invalid move")
	}
	if p.User != "" && !linuxUserRE.MatchString(p.User) {
		return errors.New("invalid user")
	}
	if p.Domain != "" && !transferHost.MatchString(p.Domain) {
		return errors.New("invalid domain")
	}
	if p.App != "" && !appNamePattern.MatchString(p.App) {
		return errors.New("invalid app")
	}
	return validTransferDBs(p.Databases, true)
}

func (p TransferDiscardParams) Validate() error {
	if !uuidPattern.MatchString(p.Move) {
		return errors.New("invalid move")
	}
	return nil
}
