// Package sensors reads live hardware telemetry from the standardized
// Linux kernel interfaces (hwmon, thermal, power_supply, drm, iio), plus
// nvidia-smi for the proprietary NVIDIA driver. Because these interfaces
// are vendor-neutral, any device with a kernel driver — Intel/AMD/ARM
// CPUs, GPUs, NVMe/SATA drives, batteries, USB sensors, EC chips — shows
// up without vendor-specific code here.
package sensors

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind classifies what a reading measures.
type Kind string

const (
	KindTemp     Kind = "temp"     // °C
	KindFan      Kind = "fan"      // RPM
	KindVoltage  Kind = "volt"     // V
	KindCurrent  Kind = "curr"     // A
	KindPower    Kind = "power"    // W
	KindUsage    Kind = "usage"    // %
	KindFreq     Kind = "freq"     // MHz
	KindCapacity Kind = "capacity" // % (battery charge)
	KindMemory   Kind = "memory"   // MB (e.g. VRAM used)
	KindHumidity Kind = "humidity" // %RH
	KindPressure Kind = "pressure" // hPa
	KindLight    Kind = "light"    // lux
)

func (k Kind) Unit() string {
	switch k {
	case KindTemp:
		return "°C"
	case KindFan:
		return "RPM"
	case KindVoltage:
		return "V"
	case KindCurrent:
		return "A"
	case KindPower:
		return "W"
	case KindUsage, KindCapacity:
		return "%"
	case KindFreq:
		return "MHz"
	case KindMemory:
		return "MB"
	case KindHumidity:
		return "%RH"
	case KindPressure:
		return "hPa"
	case KindLight:
		return "lux"
	}
	return ""
}

// Reading is one sensor value at one instant.
type Reading struct {
	Chip  string  `json:"chip"`  // device, e.g. "k10temp", "nvme: Samsung SSD 980", "battery/BAT0"
	Label string  `json:"label"` // sensor within the device, e.g. "Tctl", "fan1"
	Kind  Kind    `json:"kind"`
	Value float64 `json:"value"`
}

// Key identifies a sensor stably across samples (for history graphs).
func (r Reading) Key() string { return r.Chip + "/" + r.Label }

// Source enumerates and reads one kernel interface. Read is called every
// sample tick and re-enumerates devices, so hotplugged hardware (USB
// sensors, external drives) appears and disappears automatically.
type Source interface {
	Read() []Reading
}

// Manager fans out to all sources in parallel and merges the results.
type Manager struct {
	sources []Source
}

// NewManager wires up every source. All of them degrade to zero readings
// on hardware or kernels that lack the interface.
func NewManager() *Manager {
	return &Manager{sources: []Source{
		&Hwmon{},
		&Thermal{},
		&PowerSupply{},
		&RAPL{},
		&DRM{},
		NewNvidiaSMI(),
		&IIO{},
	}}
}

func (m *Manager) Read() []Reading {
	results := make([][]Reading, len(m.sources))
	var wg sync.WaitGroup
	for i, s := range m.sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.Read()
		}()
	}
	wg.Wait()

	var all []Reading
	for _, rs := range results {
		all = append(all, rs...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Chip != all[j].Chip {
			return all[i].Chip < all[j].Chip
		}
		return all[i].Label < all[j].Label
	})
	return all
}

// readFloat reads a sysfs attribute holding a single number.
func readFloat(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
