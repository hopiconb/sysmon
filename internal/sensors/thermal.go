package sensors

import (
	"os"
	"path/filepath"
	"strings"
)

// Thermal reads /sys/class/thermal — ACPI and SoC thermal zones. On ARM
// SoCs (Snapdragon, Rockchip, Broadcom, Apple Silicon under Asahi…) this
// is often the *primary* temperature interface: zones like cpu0-thermal,
// gpu-thermal, battery-thermal, modem exist even when hwmon is sparse.
type Thermal struct {
	root string // test override
}

func (t *Thermal) Read() []Reading {
	root := t.root
	if root == "" {
		root = "/sys/class/thermal"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	// Zones that also register a hwmon device would be reported twice;
	// the hwmon source already covers those, so skip them here.
	var out []Reading
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), "thermal_zone") {
			continue
		}
		dir := filepath.Join(root, d.Name())
		if m, _ := filepath.Glob(filepath.Join(dir, "hwmon*")); len(m) > 0 {
			continue
		}
		ztype := readString(filepath.Join(dir, "type"))
		if ztype == "" {
			continue
		}
		v, ok := readFloat(filepath.Join(dir, "temp"))
		if !ok || v <= 0 {
			continue
		}
		out = append(out, Reading{
			Chip:  "thermal",
			Label: ztype,
			Kind:  KindTemp,
			Value: v / 1000, // millidegree C
		})
	}
	return out
}
