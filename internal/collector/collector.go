// Package collector samples system-wide and per-process metrics.
package collector

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/hopiconb/sysmon/internal/sensors"
)

// Sample is one snapshot of system state. It is the wire format on the
// unix socket (NDJSON) and the unit of storage in SQLite.
type Sample struct {
	Timestamp  time.Time         `json:"ts"`
	CPUPct     float64           `json:"cpu"`
	PerCore    []float64         `json:"per_core"`
	MemPct     float64           `json:"mem"`
	MemUsedMB  float64           `json:"mem_used_mb"`
	MemTotalMB float64           `json:"mem_total_mb"`
	SwapPct    float64           `json:"swap"`
	SwapUsedMB float64           `json:"swap_used_mb"`
	Load1      float64           `json:"load1"`
	Load5      float64           `json:"load5"`
	Load15     float64           `json:"load15"`
	UptimeSec  uint64            `json:"uptime_sec"`
	NumProcs   int               `json:"num_procs"`
	NetRxKBs   float64           `json:"net_rx_kbs"` // KB/s, all interfaces except lo
	NetTxKBs   float64           `json:"net_tx_kbs"`
	DiskRKBs   float64           `json:"disk_r_kbs"` // KB/s, whole disks only
	DiskWKBs   float64           `json:"disk_w_kbs"`
	Sensors    []sensors.Reading `json:"sensors"`
	Procs      []ProcSample      `json:"procs"`
}

// MaxTemp returns the hottest temperature reading, or 0 if none.
func (s Sample) MaxTemp() float64 {
	var max float64
	for _, r := range s.Sensors {
		if r.Kind == sensors.KindTemp && r.Value > max {
			max = r.Value
		}
	}
	return max
}

type ProcSample struct {
	PID    int32   `json:"pid"`
	Name   string  `json:"name"`
	CPUPct float64 `json:"cpu"`
	MemMB  float64 `json:"mem_mb"`
}

// topN is how many processes (by CPU) are tracked when no focus list is set.
const topN = 10

// Collector holds persistent process handles between samples. gopsutil's
// CPUPercent() measures usage since the previous call on the same handle,
// so reusing handles is what makes per-process CPU numbers meaningful.
type Collector struct {
	procs   map[int32]*process.Process
	sensors *sensors.Manager

	// previous counters for rate computation
	prevT                time.Time
	prevRx, prevTx       uint64
	prevDiskR, prevDiskW uint64
}

func New() *Collector {
	return &Collector{
		procs:   map[int32]*process.Process{},
		sensors: sensors.NewManager(),
	}
}

// partitionRe matches partition device names (sda1, nvme0n1p2,
// mmcblk0p1); IO counters for those would double-count their parent disk.
var partitionRe = regexp.MustCompile(`^((s|h|v|xv)d[a-z]+\d+|(nvme|mmcblk)\d+\w*p\d+)$`)

// rates returns network and disk throughput in KB/s since the last call.
func (c *Collector) rates(ctx context.Context, now time.Time) (rx, tx, dr, dw float64) {
	var curRx, curTx uint64
	if nics, err := gnet.IOCountersWithContext(ctx, true); err == nil {
		for _, n := range nics {
			if n.Name == "lo" {
				continue
			}
			curRx += n.BytesRecv
			curTx += n.BytesSent
		}
	}
	var curDR, curDW uint64
	if disks, err := disk.IOCountersWithContext(ctx); err == nil {
		for name, d := range disks {
			if partitionRe.MatchString(name) ||
				strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") {
				continue
			}
			curDR += d.ReadBytes
			curDW += d.WriteBytes
		}
	}

	if !c.prevT.IsZero() {
		if dt := now.Sub(c.prevT).Seconds(); dt > 0 {
			rate := func(cur, prev uint64) float64 {
				if cur < prev { // counter reset (interface re-created)
					return 0
				}
				return float64(cur-prev) / dt / 1024
			}
			rx, tx = rate(curRx, c.prevRx), rate(curTx, c.prevTx)
			dr, dw = rate(curDR, c.prevDiskR), rate(curDW, c.prevDiskW)
		}
	}
	c.prevT = now
	c.prevRx, c.prevTx = curRx, curTx
	c.prevDiskR, c.prevDiskW = curDR, curDW
	return rx, tx, dr, dw
}

// Collect takes one sample. focus filters processes by case-insensitive
// substring match on the process name; an empty focus list tracks the
// top processes by CPU instead.
func (c *Collector) Collect(focus []string) (Sample, error) {
	ctx := context.Background()

	cpuPct, _ := cpu.PercentWithContext(ctx, 0, false)
	perCore, _ := cpu.PercentWithContext(ctx, 0, true)
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil {
		return Sample{}, err
	}
	readings := c.sensors.Read()
	tracked, numProcs := c.collectProcs(ctx, focus)

	var avgCPU float64
	if len(cpuPct) > 0 {
		avgCPU = cpuPct[0]
	}

	now := time.Now()
	rx, tx, dr, dw := c.rates(ctx, now)

	sm := Sample{
		Timestamp:  now,
		CPUPct:     avgCPU,
		PerCore:    perCore,
		MemPct:     vm.UsedPercent,
		MemUsedMB:  float64(vm.Used) / 1024 / 1024,
		MemTotalMB: float64(vm.Total) / 1024 / 1024,
		NumProcs:   numProcs,
		NetRxKBs:   rx,
		NetTxKBs:   tx,
		DiskRKBs:   dr,
		DiskWKBs:   dw,
		Sensors:    readings,
		Procs:      tracked,
	}
	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil {
		sm.SwapPct = sw.UsedPercent
		sm.SwapUsedMB = float64(sw.Used) / 1024 / 1024
	}
	if la, err := load.AvgWithContext(ctx); err == nil {
		sm.Load1, sm.Load5, sm.Load15 = la.Load1, la.Load5, la.Load15
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		sm.UptimeSec = up
	}
	return sm, nil
}

func (c *Collector) collectProcs(ctx context.Context, focus []string) ([]ProcSample, int) {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil, 0
	}

	alive := map[int32]bool{}
	var out []ProcSample
	for _, pid := range pids {
		alive[pid] = true
		p, ok := c.procs[pid]
		if !ok {
			p, err = process.NewProcessWithContext(ctx, pid)
			if err != nil {
				continue
			}
			c.procs[pid] = p
		}
		name, err := p.NameWithContext(ctx)
		if err != nil {
			continue
		}
		if len(focus) > 0 && !matches(name, focus) {
			continue
		}
		cp, _ := p.CPUPercentWithContext(ctx)
		var memMB float64
		if mi, err := p.MemoryInfoWithContext(ctx); err == nil && mi != nil {
			memMB = float64(mi.RSS) / 1024 / 1024
		}
		out = append(out, ProcSample{PID: pid, Name: name, CPUPct: cp, MemMB: memMB})
	}

	// Drop handles for exited processes so the map doesn't grow forever.
	for pid := range c.procs {
		if !alive[pid] {
			delete(c.procs, pid)
		}
	}

	if len(focus) == 0 {
		sort.Slice(out, func(i, j int) bool { return out[i].CPUPct > out[j].CPUPct })
		if len(out) > topN {
			out = out[:topN]
		}
	}
	return out, len(pids)
}

func matches(name string, focus []string) bool {
	lname := strings.ToLower(name)
	for _, f := range focus {
		if strings.Contains(lname, strings.ToLower(f)) {
			return true
		}
	}
	return false
}
