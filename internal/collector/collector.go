// Package collector samples system-wide and per-process metrics.
package collector

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/hopiconb/sysmon/internal/sensors"
)

// Sample is one snapshot of system state. It is the wire format on the
// unix socket (NDJSON) and the unit of storage in SQLite.
type Sample struct {
	Timestamp time.Time         `json:"ts"`
	CPUPct    float64           `json:"cpu"`
	PerCore   []float64         `json:"per_core"`
	MemPct    float64           `json:"mem"`
	MemUsedMB float64           `json:"mem_used_mb"`
	Sensors   []sensors.Reading `json:"sensors"`
	Procs     []ProcSample      `json:"procs"`
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
}

func New() *Collector {
	return &Collector{
		procs:   map[int32]*process.Process{},
		sensors: sensors.NewManager(),
	}
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
	tracked := c.collectProcs(ctx, focus)

	var avgCPU float64
	if len(cpuPct) > 0 {
		avgCPU = cpuPct[0]
	}

	return Sample{
		Timestamp: time.Now(),
		CPUPct:    avgCPU,
		PerCore:   perCore,
		MemPct:    vm.UsedPercent,
		MemUsedMB: float64(vm.Used) / 1024 / 1024,
		Sensors:   readings,
		Procs:     tracked,
	}, nil
}

func (c *Collector) collectProcs(ctx context.Context, focus []string) []ProcSample {
	pids, err := process.PidsWithContext(ctx)
	if err != nil {
		return nil
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
	return out
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
