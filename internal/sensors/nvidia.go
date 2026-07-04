package sensors

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// NvidiaSMI covers GPUs on the proprietary NVIDIA driver, which exposes
// nothing useful in sysfs. It shells out to nvidia-smi (present wherever
// that driver is installed). Auto-disabled when the binary is missing or
// keeps failing, so it costs nothing on non-NVIDIA systems.
type NvidiaSMI struct {
	path     string
	failures int
}

const nvidiaMaxFailures = 3

func NewNvidiaSMI() *NvidiaSMI {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return &NvidiaSMI{}
	}
	return &NvidiaSMI{path: path}
}

func (n *NvidiaSMI) Read() []Reading {
	if n.path == "" || n.failures >= nvidiaMaxFailures {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, n.path,
		"--query-gpu=index,name,utilization.gpu,temperature.gpu,power.draw,fan.speed,memory.used,memory.total",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		n.failures++
		return nil
	}
	n.failures = 0

	var readings []Reading
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 8 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		chip := fmt.Sprintf("gpu/nvidia%s: %s", f[0], f[1])
		add := func(label string, kind Kind, s string) {
			// nvidia-smi prints "[N/A]" for unsupported fields.
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				readings = append(readings, Reading{Chip: chip, Label: label, Kind: kind, Value: v})
			}
		}
		add("busy", KindUsage, f[2])
		add("temp", KindTemp, f[3])
		add("power", KindPower, f[4])
		add("fan", KindUsage, f[5]) // fan.speed is a percentage
		add("vram used", KindMemory, f[6])
		if used, err1 := strconv.ParseFloat(f[6], 64); err1 == nil {
			if total, err2 := strconv.ParseFloat(f[7], 64); err2 == nil && total > 0 {
				readings = append(readings, Reading{Chip: chip, Label: "vram", Kind: KindUsage, Value: used / total * 100})
			}
		}
	}
	return readings
}
