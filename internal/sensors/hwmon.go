package sensors

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Hwmon reads /sys/class/hwmon — the kernel's unified hardware-monitoring
// interface. Every hwmon driver publishes here with the same file naming
// scheme regardless of vendor: coretemp (Intel), k10temp/zenpower (AMD),
// amdgpu/nouveau (GPUs), nvme, drivetemp (SATA HDDs/SSDs), spd5118 (RAM),
// thinkpad/dell EC fans, USB hwmon devices, and so on.
type Hwmon struct {
	// root overridden in tests; empty means the real sysfs.
	root string
}

// attrClass maps a hwmon attribute prefix to its kind and the divisor
// that converts the raw integer into the kind's canonical unit.
var hwmonAttrs = []struct {
	prefix string
	kind   Kind
	div    float64
}{
	{"temp", KindTemp, 1000},         // millidegree C
	{"fan", KindFan, 1},              // RPM
	{"in", KindVoltage, 1000},        // mV
	{"curr", KindCurrent, 1000},      // mA
	{"power", KindPower, 1_000_000},  // µW
	{"freq", KindFreq, 1_000_000},    // Hz -> MHz
	{"humidity", KindHumidity, 1000}, // m%RH
}

func (h *Hwmon) Read() []Reading {
	root := h.root
	if root == "" {
		root = "/sys/class/hwmon"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Reading
	seen := map[string]int{} // disambiguate several chips with the same name
	for _, d := range dirs {
		dir := filepath.Join(root, d.Name())
		name := readString(filepath.Join(dir, "name"))
		if name == "" {
			continue
		}
		// Older kernels put the attribute files in a device/ subdir.
		attrDir := dir
		if m, _ := filepath.Glob(filepath.Join(dir, "*_input")); len(m) == 0 {
			if m, _ := filepath.Glob(filepath.Join(dir, "device", "*_input")); len(m) > 0 {
				attrDir = filepath.Join(dir, "device")
			}
		}

		chip := name
		// Drives (nvme, drivetemp) expose their model — far more useful
		// than "nvme" when several disks are attached.
		if model := readString(filepath.Join(dir, "device", "model")); model != "" {
			chip = name + ": " + model
		}
		seen[chip]++
		if n := seen[chip]; n > 1 {
			chip = fmt.Sprintf("%s #%d", chip, n)
		}

		readings := readHwmonChip(attrDir, chip)
		// Idle USB-C PD ports (ucsi) mirror all-zero electrics into
		// hwmon; the power_supply source already filters these.
		if strings.HasPrefix(name, "ucsi") && allZero(readings) {
			continue
		}
		out = append(out, readings...)
	}
	return out
}

func allZero(rs []Reading) bool {
	for _, r := range rs {
		if r.Value != 0 {
			return false
		}
	}
	return len(rs) > 0
}

func readHwmonChip(dir, chip string) []Reading {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Reading
	for _, e := range entries {
		fn := e.Name()
		base, ok := strings.CutSuffix(fn, "_input")
		if !ok {
			continue
		}
		for _, a := range hwmonAttrs {
			if !strings.HasPrefix(base, a.prefix) {
				continue
			}
			// Reject e.g. "intrusion0" matching prefix "in".
			if rest := base[len(a.prefix):]; rest == "" || strings.Trim(rest, "0123456789") != "" {
				continue
			}
			v, ok := readFloat(filepath.Join(dir, fn))
			if !ok {
				break
			}
			label := readString(filepath.Join(dir, base+"_label"))
			if label == "" {
				label = base
			}
			out = append(out, Reading{Chip: chip, Label: label, Kind: a.kind, Value: v / a.div})
			break
		}
	}
	return out
}
