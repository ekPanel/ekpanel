// Package agentproto defines the messages exchanged between the panel and server agents.
//
// Transport: the panel runs a separate TLS listener for agents (default port 7443) with its
// own private CA (see internal/pki). An agent enrols once over that listener, pinning the
// CA by fingerprint, and receives a client certificate. It then keeps a WebSocket open at
// ConnectPath using mutual TLS. Messages are JSON envelopes in both directions:
//
//	agent → panel: hello, metrics, response
//	panel → agent: request
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
)

// Hello is the first message on every connection: static facts about the server.
type Hello struct {
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Kernel       string `json:"kernel"`
	Arch         string `json:"arch"`
	CPUCount     int    `json:"cpu_count"`
	AgentVersion string `json:"agent_version"`
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

// DefaultPHPVersion is preselected for new sites.
const DefaultPHPVersion = "8.4"

type SiteSpec struct {
	ID         string   `json:"id"`
	Generation int64    `json:"generation"`
	Domain     string   `json:"domain"`
	Aliases    []string `json:"aliases"`
	Kind       string   `json:"kind"` // "static" or "php"
	PHPVersion string   `json:"php_version,omitempty"`
	TLS        string   `json:"tls"`  // "auto" (public CA) or "internal" (self-signed)
	User       string   `json:"user"` // Linux account owning the site
	Deleted    bool     `json:"deleted,omitempty"`
}

type StateApplyParams struct {
	Sites []SiteSpec `json:"sites"`
}

type SiteResult struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

type StateApplyResult struct {
	Sites []SiteResult `json:"sites"`
}
