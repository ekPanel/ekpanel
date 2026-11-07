// Package agentproto defines the messages exchanged between the panel and server agents.
//
// Transport (phase 0): the agent dials the panel over HTTPS, upgrades to a WebSocket at
// /api/v1/agent/connect and authenticates with its server ID and secret. Messages are JSON
// envelopes. Mutual TLS replaces the bearer secret later in phase 0.
package agentproto

import "time"

const (
	EnrollPath  = "/api/v1/agent/enroll"
	ConnectPath = "/api/v1/agent/connect"
)

type EnrollRequest struct {
	Token string `json:"token"`
}

type EnrollResponse struct {
	ServerID string `json:"server_id"`
	Secret   string `json:"secret"`
}

const (
	TypeHello   = "hello"
	TypeMetrics = "metrics"
)

type Envelope struct {
	Type    string   `json:"type"`
	Hello   *Hello   `json:"hello,omitempty"`
	Metrics *Metrics `json:"metrics,omitempty"`
}

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
