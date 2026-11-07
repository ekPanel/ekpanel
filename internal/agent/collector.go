// Package agent is the server-side agent: it collects facts and metrics from Linux and
// streams them to the panel.
package agent

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ekpanel/internal/agentproto"
)

// Collector reads /proc and statfs. It keeps the previous CPU and network counters,
// because both are reported as change since the last sample.
type Collector struct {
	proc     string // usually /proc; tests point it at fixtures
	diskPath string

	prevCPU cpuTimes
	prevNet netTotals
	prevAt  time.Time
}

func NewCollector(proc, diskPath string) *Collector {
	c := &Collector{proc: proc, diskPath: diskPath}
	// Prime the counters so the first real sample has a baseline.
	c.prevCPU, _ = c.readCPU()
	c.prevNet, _ = c.readNet()
	c.prevAt = time.Now()
	return c
}

func (c *Collector) Sample() (agentproto.Metrics, error) {
	now := time.Now()
	m := agentproto.Metrics{At: now.UTC()}

	cpu, err := c.readCPU()
	if err != nil {
		return m, err
	}
	m.CPUPercent = cpuPercent(c.prevCPU, cpu)
	c.prevCPU = cpu

	if err := c.readMem(&m); err != nil {
		return m, err
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(c.diskPath, &st); err != nil {
		return m, fmt.Errorf("statfs %s: %w", c.diskPath, err)
	}
	m.DiskTotal = st.Blocks * uint64(st.Bsize)
	m.DiskUsed = (st.Blocks - st.Bfree) * uint64(st.Bsize)

	net, err := c.readNet()
	if err != nil {
		return m, err
	}
	if secs := now.Sub(c.prevAt).Seconds(); secs > 0 {
		m.NetRxRate = rate(c.prevNet.rx, net.rx, secs)
		m.NetTxRate = rate(c.prevNet.tx, net.tx, secs)
	}
	c.prevNet, c.prevAt = net, now

	if b, err := os.ReadFile(filepath.Join(c.proc, "loadavg")); err == nil {
		fmt.Sscanf(string(b), "%f %f %f", &m.Load1, &m.Load5, &m.Load15)
	}
	if b, err := os.ReadFile(filepath.Join(c.proc, "uptime")); err == nil {
		var up float64
		fmt.Sscanf(string(b), "%f", &up)
		m.UptimeSeconds = uint64(up)
	}
	return m, nil
}

func rate(prev, cur uint64, secs float64) float64 {
	if cur < prev { // counter reset (interface restarted)
		return 0
	}
	return float64(cur-prev) / secs
}

// ---- CPU ----

type cpuTimes struct{ total, idle uint64 }

func (c *Collector) readCPU() (cpuTimes, error) {
	f, err := os.Open(filepath.Join(c.proc, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	return parseCPU(f)
}

// parseCPU reads the aggregate "cpu" line of /proc/stat:
// user nice system idle iowait irq softirq steal guest guest_nice.
// guest time is already included in user, so only the first eight fields are summed.
func parseCPU(r io.Reader) (cpuTimes, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var t cpuTimes
		for i, f := range fields[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("parse /proc/stat: %w", err)
			}
			t.total += v
			if i == 3 || i == 4 { // idle, iowait
				t.idle += v
			}
		}
		return t, nil
	}
	return cpuTimes{}, fmt.Errorf("parse /proc/stat: no cpu line")
}

func cpuPercent(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total {
		return 0
	}
	total := float64(cur.total - prev.total)
	idle := float64(cur.idle - prev.idle)
	return (total - idle) / total * 100
}

// ---- Memory ----

func (c *Collector) readMem(m *agentproto.Metrics) error {
	f, err := os.Open(filepath.Join(c.proc, "meminfo"))
	if err != nil {
		return err
	}
	defer f.Close()
	return parseMem(f, m)
}

// parseMem fills memory fields from /proc/meminfo. "Used" is total minus MemAvailable,
// which is what people mean by used: page cache the kernel can drop counts as free.
func parseMem(r io.Reader, m *agentproto.Metrics) error {
	vals := map[string]uint64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		vals[key] = v * 1024 // values are in kB
	}
	total, ok := vals["MemTotal"]
	if !ok {
		return fmt.Errorf("parse /proc/meminfo: no MemTotal")
	}
	avail, ok := vals["MemAvailable"]
	if !ok {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	m.MemTotal = total
	if avail < total {
		m.MemUsed = total - avail
	}
	m.SwapTotal = vals["SwapTotal"]
	if free := vals["SwapFree"]; free < m.SwapTotal {
		m.SwapUsed = m.SwapTotal - free
	}
	return nil
}

// ---- Network ----

type netTotals struct{ rx, tx uint64 }

func (c *Collector) readNet() (netTotals, error) {
	f, err := os.Open(filepath.Join(c.proc, "net", "dev"))
	if err != nil {
		return netTotals{}, err
	}
	defer f.Close()
	return parseNet(f)
}

// virtualInterface reports interfaces whose traffic is already counted on a physical one
// (container veths and bridges), or that never leave the machine.
func virtualInterface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "virbr", "cni", "flannel", "tun", "tap"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseNet sums received and transmitted bytes over physical interfaces in /proc/net/dev.
func parseNet(r io.Reader) (netTotals, error) {
	var t netTotals
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if virtualInterface(name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[0], 10, 64)
		tx, err2 := strconv.ParseUint(fields[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		t.rx += rx
		t.tx += tx
	}
	return t, sc.Err()
}

// ---- Facts ----

func Facts(version string) agentproto.Hello {
	h := agentproto.Hello{Arch: runtime.GOARCH, CPUCount: runtime.NumCPU(), AgentVersion: version}
	h.Hostname, _ = os.Hostname()
	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(string(b))
	}
	if f, err := os.Open("/etc/os-release"); err == nil {
		h.OS = osName(f)
		f.Close()
	}
	return h
}

// osName returns PRETTY_NAME from an os-release file, e.g. "Ubuntu 24.04.1 LTS".
func osName(r io.Reader) string {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
