package agent

import (
	"math"
	"strings"
	"testing"

	"ekpanel/internal/agentproto"
)

func TestParseCPUAndPercent(t *testing.T) {
	before, err := parseCPU(strings.NewReader("cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	// 200 more busy jiffies, 200 more idle: 50% used. Guest (50) must not be double counted.
	after, err := parseCPU(strings.NewReader("cpu  250 0 150 850 150 0 0 0 90 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if before.total != 1000 || before.idle != 800 {
		t.Fatalf("before = %+v", before)
	}
	if got := cpuPercent(before, after); math.Abs(got-50) > 0.001 {
		t.Fatalf("cpu = %v, want 50", got)
	}
	if got := cpuPercent(after, before); got != 0 {
		t.Fatalf("going backwards must report 0, got %v", got)
	}
}

func TestParseMem(t *testing.T) {
	const meminfo = `MemTotal:        8000000 kB
MemFree:          500000 kB
MemAvailable:    6000000 kB
Buffers:          100000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`
	var m agentproto.Metrics
	if err := parseMem(strings.NewReader(meminfo), &m); err != nil {
		t.Fatal(err)
	}
	if m.MemTotal != 8000000*1024 || m.MemUsed != 2000000*1024 {
		t.Fatalf("mem total/used = %d/%d", m.MemTotal, m.MemUsed)
	}
	if m.SwapTotal != 2000000*1024 || m.SwapUsed != 500000*1024 {
		t.Fatalf("swap total/used = %d/%d", m.SwapTotal, m.SwapUsed)
	}
}

func TestParseNetSkipsVirtualInterfaces(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999     100    0    0    0     0          0         0  9999999     100    0    0    0     0       0          0
  eth0: 1000        10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
  ens4: 500          5    0    0    0     0          0         0      700       7    0    0    0     0       0          0
docker0: 88888       1    0    0    0     0          0         0    88888       1    0    0    0     0       0          0
vethab12: 77777      1    0    0    0     0          0         0    77777       1    0    0    0     0       0          0
`
	got, err := parseNet(strings.NewReader(dev))
	if err != nil {
		t.Fatal(err)
	}
	if got.rx != 1500 || got.tx != 2700 {
		t.Fatalf("got %+v, want rx 1500 tx 2700", got)
	}
}

func TestOSName(t *testing.T) {
	got := osName(strings.NewReader("NAME=\"Ubuntu\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n"))
	if got != "Ubuntu 24.04.1 LTS" {
		t.Fatalf("got %q", got)
	}
}

func TestSampleOnThisMachine(t *testing.T) {
	c := NewCollector("/proc", "/")
	m, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if m.MemTotal == 0 || m.DiskTotal == 0 || m.CPUPercent < 0 || m.CPUPercent > 100 {
		t.Fatalf("implausible sample: %+v", m)
	}
}
