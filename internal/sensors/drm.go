package sensors

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DRM reads /sys/class/drm for GPU load and memory. Temperatures, fans,
// and power for these GPUs already arrive via their hwmon interface; this
// source adds the bits hwmon doesn't carry:
//   - amdgpu: gpu_busy_percent, VRAM used/total
//   - Intel i915/xe: current GT frequency
//   - nouveau: (hwmon only — nothing extra here)
//
// Vendor is identified by PCI id, so any discrete or integrated GPU with
// a kernel driver is picked up.
type DRM struct {
	root string // test override
}

var cardRe = regexp.MustCompile(`^card[0-9]+$`)

func (g *DRM) Read() []Reading {
	root := g.root
	if root == "" {
		root = "/sys/class/drm"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Reading
	for _, d := range dirs {
		if !cardRe.MatchString(d.Name()) {
			continue
		}
		card := filepath.Join(root, d.Name())
		dev := filepath.Join(card, "device")
		vendor := readString(filepath.Join(dev, "vendor"))

		var chip string
		switch vendor {
		case "0x1002":
			chip = "gpu/amd-" + d.Name()
		case "0x8086":
			chip = "gpu/intel-" + d.Name()
		case "0x10de":
			chip = "gpu/nvidia-" + d.Name() // nouveau; proprietary handled by nvidia-smi
		case "":
			continue
		default:
			chip = "gpu/" + strings.TrimPrefix(vendor, "0x") + "-" + d.Name()
		}

		if v, ok := readFloat(filepath.Join(dev, "gpu_busy_percent")); ok {
			out = append(out, Reading{Chip: chip, Label: "busy", Kind: KindUsage, Value: v})
		}
		if used, ok := readFloat(filepath.Join(dev, "mem_info_vram_used")); ok {
			out = append(out, Reading{Chip: chip, Label: "vram used", Kind: KindMemory, Value: used / 1024 / 1024})
			if total, ok := readFloat(filepath.Join(dev, "mem_info_vram_total")); ok && total > 0 {
				out = append(out, Reading{Chip: chip, Label: "vram", Kind: KindUsage, Value: used / total * 100})
			}
		}
		// Intel GT frequency: newer kernels under gt/gt0, older at card root.
		for _, p := range []string{
			filepath.Join(card, "gt", "gt0", "rps_cur_freq_mhz"),
			filepath.Join(card, "gt_cur_freq_mhz"),
		} {
			if v, ok := readFloat(p); ok {
				out = append(out, Reading{Chip: chip, Label: "gt freq", Kind: KindFreq, Value: v})
				break
			}
		}
	}
	return out
}
