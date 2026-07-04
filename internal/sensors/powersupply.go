package sensors

import (
	"os"
	"path/filepath"
)

// PowerSupply reads /sys/class/power_supply — laptop batteries, AC
// adapters, USB-C PD, and even wireless peripherals that report charge
// (mice, keyboards, controllers register here via hid drivers).
type PowerSupply struct {
	root string // test override
}

func (p *PowerSupply) Read() []Reading {
	root := p.root
	if root == "" {
		root = "/sys/class/power_supply"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Reading
	for _, d := range dirs {
		dir := filepath.Join(root, d.Name())
		chip := "power/" + d.Name()

		add := func(label string, kind Kind, v float64) {
			out = append(out, Reading{Chip: chip, Label: label, Kind: kind, Value: v})
		}

		if v, ok := readFloat(filepath.Join(dir, "capacity")); ok {
			add("charge", KindCapacity, v)
		}
		if v, ok := readFloat(filepath.Join(dir, "temp")); ok {
			add("temp", KindTemp, v/10) // tenths of °C
		}
		volt, hasVolt := readFloat(filepath.Join(dir, "voltage_now")) // µV
		if hasVolt {
			add("voltage", KindVoltage, volt/1e6)
		}
		curr, hasCurr := readFloat(filepath.Join(dir, "current_now")) // µA
		if hasCurr {
			add("current", KindCurrent, curr/1e6)
		}
		if v, ok := readFloat(filepath.Join(dir, "power_now")); ok { // µW
			add("power", KindPower, v/1e6)
		} else if hasVolt && hasCurr {
			// Batteries that report V+A but not W: derive the draw.
			add("power", KindPower, volt/1e6*curr/1e6)
		}
	}
	return out
}
