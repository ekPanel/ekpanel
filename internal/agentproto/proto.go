// Package agentproto defines the messages exchanged between the panel and server agents.
//
// Transport: the panel runs a separate TLS listener for agents (default port 7443) with its
// own private CA (see internal/pki). An agent enrols once over that listener, pinning the
// CA by fingerprint, and receives a client certificate. It then keeps a WebSocket open at
// ConnectPath using mutual TLS. Messages are JSON envelopes in both directions:
//
//	agent → panel: hello, metrics, response, stream data and close
//	panel → agent: request, stream data, resize and close
package agentproto

import (
	"encoding/json"
	"time"
)

const (
	EnrollPath  = "/api/v1/agent/enroll"
	RenewPath   = "/api/v1/agent/renew"
	ConnectPath = "/api/v1/agent/connect"

	// DefaultPort is where the panel listens for agents.
	DefaultPort = 7443
)

type EnrollRequest struct {
	Token  string `json:"token"`
	CSRPEM string `json:"csr_pem"`
}

type EnrollResponse struct {
	ServerID string `json:"server_id"`
	CertPEM  string `json:"cert_pem"`
}

// RenewRequest is sent over mutual TLS with the agent's current certificate.
type RenewRequest struct {
	CSRPEM string `json:"csr_pem"`
}

type RenewResponse struct {
	CertPEM string `json:"cert_pem"`
}

const (
	TypeHello    = "hello"
	TypeMetrics  = "metrics"
	TypeRequest  = "request"
	TypeResponse = "response"
	// A stream is a long-lived byte channel, such as a terminal, opened by a request whose
	// params carry the stream's ID. Its traffic then flows in these envelopes.
	TypeStreamData   = "stream_data"
	TypeStreamResize = "stream_resize"
	TypeStreamClose  = "stream_close"
)

type Envelope struct {
	Type    string   `json:"type"`
	Hello   *Hello   `json:"hello,omitempty"`
	Metrics *Metrics `json:"metrics,omitempty"`

	// Request/response fields. ID pairs a response with its request.
	ID     string          `json:"id,omitempty"`
	Op     string          `json:"op,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *OpError        `json:"error,omitempty"`

	// Stream fields.
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	Cols   int    `json:"cols,omitempty"`
	Rows   int    `json:"rows,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// OpError is a failed operation. Code is stable (the UI translates it); Message is detail.
type OpError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *OpError) Error() string { return e.Code + ": " + e.Message }

// Operation error codes.
const (
	ErrUnknownOp     = "unknown_op"
	ErrInvalidParams = "invalid_params"
	ErrNotAllowed    = "not_allowed"
	ErrFailed        = "failed"
	ErrNoSystemd     = "no_systemd" // the server (or container) does not run systemd
	ErrWrongKey      = "wrong_recovery_key"
)

// Hello is the first message on every connection: static facts about the server.
type Hello struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel"`
	Arch         string `json:"arch"`
	CPUCount     int    `json:"cpu_count"`
	AgentVersion string `json:"agent_version"`
	// Addresses are the server's global unicast IP addresses.
	Addresses []string `json:"addresses,omitempty"`
}

// Metrics is one sample of live server usage. Byte values are bytes, rates are bytes/second.
type Metrics struct {
	At            time.Time `json:"at"`
	CPUPercent    float64   `json:"cpu_percent"`
	Load1         float64   `json:"load1"`
	Load5         float64   `json:"load5"`
	Load15        float64   `json:"load15"`
	MemTotal      uint64    `json:"mem_total"`
	MemUsed       uint64    `json:"mem_used"`
	SwapTotal     uint64    `json:"swap_total"`
	SwapUsed      uint64    `json:"swap_used"`
	DiskTotal     uint64    `json:"disk_total"`
	DiskUsed      uint64    `json:"disk_used"`
	NetRxRate     float64   `json:"net_rx_rate"`
	NetTxRate     float64   `json:"net_tx_rate"`
	UptimeSeconds uint64    `json:"uptime_seconds"`
}

// MetricsInterval is how often agents send a sample.
const MetricsInterval = 2 * time.Second

// ---- Operations ----
//
// Every operation the panel can ask an agent to perform is listed here with typed
// parameters and results. Agents reject anything else; there is no generic "run command".

const (
	OpServicesStatus  = "services.status"
	OpServicesRestart = "services.restart"
)

// Service is one systemd unit as seen by services.status.
type Service struct {
	Name        string     `json:"name"`
	Installed   bool       `json:"installed"`
	ActiveState string     `json:"active_state"` // active, inactive, failed, activating, …
	SubState    string     `json:"sub_state"`    // running, dead, exited, …
	Since       *time.Time `json:"since"`
	Restartable bool       `json:"restartable"`
}

type ServicesStatusResult struct {
	Services []Service `json:"services"`
}

type ServicesRestartParams struct {
	Name string `json:"name"`
}

// ---- Desired state ----

// OpStateApply sends the full desired state of a server; the agent converges to it and
// reports a result per item. The panel always sends every site of the server, including
// those being deleted (Deleted=true), so the agent never guesses what to remove.
const OpStateApply = "state.apply"

// PHPVersions the panel offers, newest first. Agents install a version when a site needs it.
var PHPVersions = []string{"8.5", "8.4", "8.3", "8.2", "8.1", "7.4"}

// OpPHPInfo lists the PHP versions installed on the server and their extensions.
// OpPHPExtension installs or removes one extension of one version.
const (
	OpPHPInfo      = "php.info"
	OpPHPExtension = "php.extension"
)

// RequiredPHPExtensions are installed with every PHP version and cannot be removed.
var RequiredPHPExtensions = []string{"cli", "common", "fpm", "mysql", "pgsql", "curl", "gd", "mbstring", "xml", "zip", "intl", "bcmath", "opcache"}

// OptionalPHPExtensions can be added and removed, as php<version>-<name> packages.
var OptionalPHPExtensions = []string{"apcu", "bz2", "gmp", "igbinary", "imagick", "imap", "ldap", "mailparse", "memcached",
	"mongodb", "msgpack", "redis", "soap", "sqlite3", "tidy", "xdebug", "yaml"}

type PHPExtensionParams struct {
	Version   string `json:"version"`
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
}

type PHPInfoResult struct {
	Versions []PHPVersionInfo `json:"versions"`
}

type PHPVersionInfo struct {
	Version    string             `json:"version"`
	Extensions []PHPExtensionInfo `json:"extensions"`
}

type PHPExtensionInfo struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Required  bool   `json:"required,omitempty"`
}

// DefaultPHPVersion is preselected for new sites.
const DefaultPHPVersion = "8.4"

type SiteSpec struct {
	ID         string   `json:"id"`
	Generation int64    `json:"generation"`
	Domain     string   `json:"domain"`
	Aliases    []string `json:"aliases"`
	Kind       string   `json:"kind"` // "static" or "php"
	PHPVersion string   `json:"php_version,omitempty"`
	TLS        string   `json:"tls"`             // "auto" (public CA), "internal" (self-signed) or "custom"
	User       string   `json:"user"`            // Linux account owning the site
	Owner      string   `json:"owner,omitempty"` // customer ID; their processes run in their slice
	Deleted    bool     `json:"deleted,omitempty"`

	// SFTP sign-in for the site's account, locked into its home folder.
	SFTPEnabled      bool     `json:"sftp_enabled,omitempty"`
	SFTPPasswordHash string   `json:"sftp_password_hash,omitempty"` // SHA-512 crypt; empty means keys only
	SSHKeys          []string `json:"ssh_keys,omitempty"`           // public keys, one per entry

	Web SiteWeb `json:"web"`
	// With TLS "custom": the uploaded certificate chain and its key, in PEM.
	CertPEM string `json:"cert_pem,omitempty"`
	KeyPEM  string `json:"key_pem,omitempty"`
}

// DatabaseSpec is one customer database with its owner account. PasswordHash is the
// engine's own hash (MariaDB native hash or PostgreSQL SCRAM verifier), never a password.
type DatabaseSpec struct {
	ID           string `json:"id"`
	Generation   int64  `json:"generation"`
	Engine       string `json:"engine"` // "mariadb" or "postgresql"
	Name         string `json:"name"`
	User         string `json:"user"`
	PasswordHash string `json:"password_hash"`
	Deleted      bool   `json:"deleted,omitempty"`
}

type StateApplyParams struct {
	Sites     []SiteSpec     `json:"sites"`
	Databases []DatabaseSpec `json:"databases"`
	Apps      []AppSpec      `json:"apps"`
	Cron      []CronSpec     `json:"cron"`
	// Owners carries the limits of every customer that owns something on the server.
	Owners []OwnerSpec `json:"owners"`
	// Firewall is nil when ekPanel does not manage the server's firewall.
	Firewall *FirewallSpec `json:"firewall,omitempty"`
	// Mail is nil when the server has no email.
	Mail *MailSpec `json:"mail,omitempty"`
}

// AppSpec is one Docker app: a Compose project named "ekpanel-<Name>" kept in
// /srv/ekpanel/apps/<Name>. Image apps get a Compose file generated by the agent; Compose
// apps bring their own. If ContainerPort is set, that port of Service is published on
// 127.0.0.1:HostPort only, and Domain (if any) is proxied to it by Caddy.
type AppSpec struct {
	ID            string   `json:"id"`
	Generation    int64    `json:"generation"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`            // "image", "compose" or "git"
	Owner         string   `json:"owner,omitempty"` // customer ID; containers run in their slice
	Image         string   `json:"image,omitempty"`
	Compose       string   `json:"compose,omitempty"`
	Service       string   `json:"service,omitempty"`
	ContainerPort int      `json:"container_port,omitempty"`
	HostPort      int      `json:"host_port,omitempty"`
	Domain        string   `json:"domain,omitempty"`
	TLS           string   `json:"tls,omitempty"`
	Env           []EnvVar `json:"env,omitempty"`
	Volumes       []string `json:"volumes,omitempty"` // container paths kept in named volumes (image apps)
	MemoryMB      int      `json:"memory_mb,omitempty"`
	CPUs          float64  `json:"cpus,omitempty"`
	// PublicPorts lets a Compose file publish ports on public addresses. Docker opens those
	// past the firewall, so it is off unless the user chose it.
	PublicPorts bool `json:"public_ports,omitempty"`
	Stopped     bool `json:"stopped,omitempty"`
	Deleted     bool `json:"deleted,omitempty"`
	// KeepImages are the builds of a Git app kept for rollbacks; the agent removes others.
	KeepImages []string `json:"keep_images,omitempty"`
	DeleteData bool     `json:"delete_data,omitempty"` // with Deleted: remove volumes and the app folder too
}

// OwnerSpec is a customer's limits, applied to a systemd slice that holds their PHP-FPM
// and containers. Zero means no limit.
type OwnerSpec struct {
	ID         string `json:"id"`
	MemoryMB   int    `json:"memory_mb,omitempty"`
	CPUPercent int    `json:"cpu_percent,omitempty"` // 100 = one core
}

type EnvVar struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// AppServiceName is the Compose service of an image app.
const AppServiceName = "app"

// FirewallSpec is the whole ufw firewall ekPanel keeps. With Enabled, the agent always allows
// SSH (on the ports sshd listens on), HTTP, HTTPS and the agent port before any rule of the user.
type FirewallSpec struct {
	Enabled bool               `json:"enabled"`
	Rules   []FirewallRuleSpec `json:"rules"`
	Mail    bool               `json:"mail,omitempty"` // also open MailPorts
}

type FirewallRuleSpec struct {
	ID       string `json:"id"`
	Action   string `json:"action"`   // "allow" or "deny"
	Protocol string `json:"protocol"` // "tcp", "udp" or "both"
	PortFrom int    `json:"port_from,omitempty"`
	PortTo   int    `json:"port_to,omitempty"`
	Source   string `json:"source,omitempty"` // address or CIDR; empty means anywhere
}

type FirewallResult struct {
	Active   bool   `json:"active"`
	SSHPorts []int  `json:"ssh_ports"`
	Error    string `json:"error,omitempty"`
}

type SiteResult struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

type DatabaseResult struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	SizeBytes  *int64 `json:"size_bytes,omitempty"`
}

type StateApplyResult struct {
	Sites     []SiteResult     `json:"sites"`
	Databases []DatabaseResult `json:"databases"`
	Apps      []AppResult      `json:"apps"`
	Cron      []AppResult      `json:"cron"`
	Firewall  *FirewallResult  `json:"firewall,omitempty"`
	Mail      *MailResult      `json:"mail,omitempty"`
}

type AppResult struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// ---- Files ----

// OpFiles runs one file manager action inside a site's home folder. The agent performs it
// in a separate process that is chrooted into the folder and runs as the site's account,
// so the kernel stops it from reaching anything else, symlinks included.
const OpFiles = "files"

// FileChunkSize is the largest piece of a file sent in one message (before base64).
const FileChunkSize = 512 << 10

// FileEditLimit is the largest file the editor opens or saves.
const FileEditLimit = 2 << 20

type FilesParams struct {
	User   string `json:"user"`   // site account
	Action string `json:"action"` // list, read, write, mkdir, rename, delete, chmod, extract, compress, read_chunk, write_chunk, empty_trash
	// Paths are relative to the site's home folder, written with a leading "/".
	Path    string   `json:"path,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	To      string   `json:"to,omitempty"`
	Data    []byte   `json:"data,omitempty"` // file content for write and write_chunk
	Offset  int64    `json:"offset,omitempty"`
	Final   bool     `json:"final,omitempty"` // last chunk of an upload: move it into place
	Mode    string   `json:"mode,omitempty"`  // octal permissions for chmod, e.g. "644"
	Replace bool     `json:"replace,omitempty"`
}

type FileEntry struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"` // file, dir or link
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"`
	Modified time.Time `json:"modified"`
	Target   string    `json:"target,omitempty"` // for links
}

type FilesResult struct {
	Entries []FileEntry `json:"entries,omitempty"`
	Data    []byte      `json:"data,omitempty"`
	Size    int64       `json:"size,omitempty"` // total size for read_chunk
	EOF     bool        `json:"eof,omitempty"`
	Path    string      `json:"path,omitempty"` // where something was put (trash, archive)
	Count   int         `json:"count,omitempty"`
}

// ---- Applications ----

// OpWordPressInstall installs WordPress into an empty site with WP-CLI, running as the
// site's account. Passwords travel only in this message and reach WP-CLI through stdin.
const OpWordPressInstall = "wordpress.install"

type WordPressInstallParams struct {
	User          string `json:"user"`
	Domain        string `json:"domain"`
	PHPVersion    string `json:"php_version"`
	DBName        string `json:"db_name"`
	DBUser        string `json:"db_user"`
	DBPassword    string `json:"db_password"`
	Title         string `json:"title"`
	AdminUser     string `json:"admin_user"`
	AdminPassword string `json:"admin_password"`
	AdminEmail    string `json:"admin_email"`
	Locale        string `json:"locale"` // WordPress locale such as en_US or bn_BD
}

type WordPressInstallResult struct {
	Version string `json:"version"`
}

// ---- Backups (restic) ----
//
// A repository's encryption password and storage credentials live only on the agent
// (/etc/ekpanel/backup). backup.setup returns the password once, as a recovery key.

const (
	OpBackupSetup     = "backup.setup"
	OpBackupRun       = "backup.run"
	OpBackupSnapshots = "backup.snapshots"
	OpBackupRestore   = "backup.restore"
	OpBackupRemove    = "backup.remove"
)

type BackupS3 struct {
	Endpoint  string `json:"endpoint"` // host[:port], or http(s)://host[:port]
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	Region    string `json:"region,omitempty"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

type BackupSetupParams struct {
	RepoID string    `json:"repo_id"`
	Kind   string    `json:"kind"` // "local" or "s3"
	S3     *BackupS3 `json:"s3,omitempty"`
	// RecoveryKey connects an existing repository (for example after moving to a new
	// server). Empty creates a new repository with a new key.
	RecoveryKey string `json:"recovery_key,omitempty"`
}

type BackupSetupResult struct {
	RecoveryKey string `json:"recovery_key,omitempty"` // only for a new repository
	Location    string `json:"location"`
}

type BackupSite struct {
	ID   string `json:"id"`
	Name string `json:"name"` // domain, for people reading the snapshot list
	User string `json:"user"`
}

type BackupDatabase struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Engine string `json:"engine"`
	Owner  string `json:"owner"` // PostgreSQL restores objects as this role
}

type BackupRunParams struct {
	RepoID      string           `json:"repo_id"`
	Sites       []BackupSite     `json:"sites"`
	Databases   []BackupDatabase `json:"databases"`
	KeepDaily   int              `json:"keep_daily"`
	KeepWeekly  int              `json:"keep_weekly"`
	KeepMonthly int              `json:"keep_monthly"`
}

type BackupItemResult struct {
	Kind       string `json:"kind"` // "site" or "database"
	ID         string `json:"id"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	SnapshotID string `json:"snapshot_id,omitempty"`
}

type BackupRunResult struct {
	Items      []BackupItemResult `json:"items"`
	PruneError string             `json:"prune_error,omitempty"`
}

type BackupSnapshot struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	ItemID string    `json:"item_id"`
	Name   string    `json:"name"`
}

type BackupSnapshotsParams struct {
	RepoID string `json:"repo_id"`
}

type BackupSnapshotsResult struct {
	Snapshots []BackupSnapshot `json:"snapshots"`
}

type BackupRestoreParams struct {
	RepoID     string          `json:"repo_id"`
	SnapshotID string          `json:"snapshot_id"`
	Site       *BackupSite     `json:"site,omitempty"`
	Database   *BackupDatabase `json:"database,omitempty"`
	// Mode for sites: "folder" restores next to the live files; "replace" swaps them in and
	// moves the current files to the site's trash.
	Mode string `json:"mode,omitempty"`
}

type BackupRestoreResult struct {
	Path string `json:"path,omitempty"`
}

type BackupRemoveParams struct {
	RepoID string `json:"repo_id"`
}

// ---- Apps ----

// Operations on one app by name. The agent refuses apps it did not set up.
const (
	OpAppStatus  = "app.status"  // containers and their state
	OpAppLogs    = "app.logs"    // recent log lines
	OpAppRestart = "app.restart" // restart every container
	OpAppPull    = "app.pull"    // pull newer images and recreate what changed
)

type AppParams struct {
	Name    string `json:"name"`
	Service string `json:"service,omitempty"` // logs: one service only
	Tail    int    `json:"tail,omitempty"`    // logs: number of lines per container
}

type AppContainer struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   string `json:"state"`  // running, exited, restarting…
	Status  string `json:"status"` // "Up 3 minutes (healthy)"
	Health  string `json:"health,omitempty"`
}

type AppStatusResult struct {
	Containers []AppContainer `json:"containers"`
}

type AppLogsResult struct {
	Logs      string `json:"logs"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ---- Disk use ----

// OpDiskUsage measures the disk space of sites (their home folder) and apps (their folder
// and volumes), in bytes actually allocated.
const OpDiskUsage = "usage.disk"

type DiskUsageParams struct {
	Sites []DiskUsageSite `json:"sites"`
	Apps  []DiskUsageApp  `json:"apps"`
}

type DiskUsageSite struct {
	ID   string `json:"id"`
	User string `json:"user"`
}

type DiskUsageApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type DiskUsageResult struct {
	Sites map[string]int64 `json:"sites"`
	Apps  map[string]int64 `json:"apps"`
}

// ---- Cron jobs ----

// CronSpec is one scheduled command, run by a systemd timer. A website's job runs as the
// site's account (User); an app's job runs inside a container of the app (App, Service).
type CronSpec struct {
	ID             string `json:"id"`
	Generation     int64  `json:"generation"`
	Owner          string `json:"owner,omitempty"` // customer ID: the job runs in their slice
	User           string `json:"user,omitempty"`
	App            string `json:"app,omitempty"`
	Service        string `json:"service,omitempty"`
	Schedule       string `json:"schedule"` // five-field cron expression
	Timezone       string `json:"timezone"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Enabled        bool   `json:"enabled"`
	Deleted        bool   `json:"deleted,omitempty"`
}

const (
	OpCronRun    = "cron.run"    // start a job now
	OpCronStatus = "cron.status" // last and next run, and recent output
)

type CronParams struct {
	ID string `json:"id"`
}

type CronStatusResult struct {
	Running  bool       `json:"running"`
	LastRun  *time.Time `json:"last_run,omitempty"`
	LastEnd  *time.Time `json:"last_end,omitempty"`
	ExitCode *int       `json:"exit_code,omitempty"`
	Result   string     `json:"result,omitempty"` // success, exit-code, timeout, signal…
	NextRun  *time.Time `json:"next_run,omitempty"`
	Output   string     `json:"output"`
}

// ---- Web logs and traffic ----

// OpAccessLog reads the newest requests of a website or app from its web server log.
const OpAccessLog = "logs.access"

type AccessLogParams struct {
	Kind   string `json:"kind"` // "site" or "app"
	ID     string `json:"id"`
	Cursor int64  `json:"cursor,omitempty"` // continue after this byte; 0 reads the newest lines
	Lines  int    `json:"lines,omitempty"`
}

type AccessLogEntry struct {
	Time       time.Time `json:"time"`
	IP         string    `json:"ip"`
	Method     string    `json:"method"`
	Host       string    `json:"host"`
	URI        string    `json:"uri"`
	Proto      string    `json:"proto"`
	Status     int       `json:"status"`
	Size       int64     `json:"size"`
	DurationMS float64   `json:"duration_ms"`
	UserAgent  string    `json:"user_agent,omitempty"`
	Referer    string    `json:"referer,omitempty"`
}

type AccessLogResult struct {
	Entries []AccessLogEntry `json:"entries"`
	Cursor  int64            `json:"cursor"`
}

// OpTraffic adds the requests logged since the last call to hourly totals the agent keeps
// for three days, and returns the hours from Since on. Asking again returns the same totals.
const OpTraffic = "traffic.collect"

type TrafficParams struct {
	Sites []string  `json:"sites"`
	Apps  []string  `json:"apps"`
	Since time.Time `json:"since"`
}

type TrafficBucket struct {
	Kind     string           `json:"kind"` // "site" or "app"
	ID       string           `json:"id"`
	Hour     time.Time        `json:"hour"`
	Requests int64            `json:"requests"`
	Bytes    int64            `json:"bytes"`
	Status   [4]int64         `json:"status"` // 2xx, 3xx, 4xx, 5xx
	Paths    map[string]int64 `json:"paths"`  // the most requested paths
}

type TrafficResult struct {
	Buckets []TrafficBucket `json:"buckets"`
}

// ---- Terminals ----

// OpTerminalOpen starts a shell in a pseudo-terminal and streams it as Stream. Kind "server"
// is a root login shell on the server; "container" is a shell inside a container of an app.
const OpTerminalOpen = "terminal.open"

type TerminalParams struct {
	Stream  string `json:"stream"`
	Kind    string `json:"kind"`
	App     string `json:"app,omitempty"`
	Service string `json:"service,omitempty"`
	Cols    int    `json:"cols"`
	Rows    int    `json:"rows"`
}

// OpAppBuild clones a Git app's repository and builds its image, streaming the output as
// Stream. The agent ends the stream with reason "exit" and a BuildResult when the image is
// built, or with the error. Closing the stream from the panel cancels the build.
const OpAppBuild = "app.build"

type BuildParams struct {
	Stream string `json:"stream"`
	App    string `json:"app"`   // the app's name
	Image  string `json:"image"` // the tag to build, ekpanel/<app>:<deployment>
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit,omitempty"` // build this commit of the branch, if given
	// Ref is fetched instead of the branch, for a pull request (refs/pull/<n>/head).
	Ref string `json:"ref,omitempty"`
	// Builder is "dockerfile", "railpack" (no Dockerfile needed), or "auto": the
	// Dockerfile if the repository has one, otherwise Railpack.
	Builder    string `json:"builder"`
	Dockerfile string `json:"dockerfile"`
	Context    string `json:"context"`
	// SSHKey is the app's private deploy key, for repositories reached over SSH.
	SSHKey string `json:"ssh_key,omitempty"`
	// Env is passed as build arguments, which a Dockerfile can read with ARG.
	Env   []EnvVar `json:"env,omitempty"`
	Owner string   `json:"owner,omitempty"`
}

type BuildResult struct {
	Commit  string `json:"commit"`
	Message string `json:"message"`
}

// GitImageRepo is the local image repository of a Git app's builds.
func GitImageRepo(app string) string { return "ekpanel/" + app }

// MaxStreamChunk is the most bytes one stream_data envelope carries.
const MaxStreamChunk = 16 << 10
