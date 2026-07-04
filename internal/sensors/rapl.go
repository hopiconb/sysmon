package sensors

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RAPL reads /sys/class/powercap — live CPU/platform power on Intel and
// AMD (both use the intel-rapl driver). Domains: package-N (CPU socket),
// core, uncore, dram, and psys (whole platform, where supported). The
// counters are monotonic energy in µJ, so wattage is the delta between
// samples.
//
// Kernels since ~5.10 restrict energy_uj to root (PLATYPUS mitigation);
// this source silently activates when the files are readable — see the
// README for the one-line udev/tmpfiles unlock.
type RAPL struct {
	root string // test override
	prev map[string]raplPrev
}

type raplPrev struct {
	uj float64
	t  time.Time
}

func (r *RAPL) Read() []Reading {
	root := r.root
	if root == "" {
		root = "/sys/class/powercap"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	if r.prev == nil {
		r.prev = map[string]raplPrev{}
	}

	var out []Reading
	seen := map[string]int{}
	for _, d := range dirs {
		// intel-rapl-mmio:* duplicates the intel-rapl:* package domain.
		if !strings.HasPrefix(d.Name(), "intel-rapl") || strings.HasPrefix(d.Name(), "intel-rapl-mmio") {
			continue
		}
		dir := filepath.Join(root, d.Name())
		label := readString(filepath.Join(dir, "name"))
		if label == "" {
			continue
		}
		uj, ok := readFloat(filepath.Join(dir, "energy_uj"))
		if !ok {
			continue // unreadable (root-only) or absent
		}
		now := time.Now()
		prev, has := r.prev[dir]
		r.prev[dir] = raplPrev{uj: uj, t: now}
		if !has {
			continue // first sample: no delta yet
		}
		dt := now.Sub(prev.t).Seconds()
		if dt <= 0 {
			continue
		}
		delta := uj - prev.uj
		if delta < 0 {
			// Counter wrapped around its max range.
			if max, ok := readFloat(filepath.Join(dir, "max_energy_range_uj")); ok {
				delta += max
			} else {
				continue
			}
		}
		watts := delta / 1e6 / dt
		if watts < 0 || watts > 100000 {
			continue
		}
		seen[label]++
		if n := seen[label]; n > 1 {
			label = label + " #" + string(rune('0'+n))
		}
		out = append(out, Reading{Chip: "power/rapl", Label: label, Kind: KindPower, Value: watts})
	}
	return out
}
