package sensors

import (
	"math"
	"os"
	"path/filepath"
)

// PowerSupply reads /sys/class/power_supply — laptop batteries, AC
// adapters, USB-C PD, and even wireless peripherals that report charge
// (mice, keyboards, controllers register here via hid drivers).
//
// Battery gauges vary by hardware: some report power_now/energy_now (µW,
// energy-based), others current_now/charge_now (µA, charge-based), and
// current can be negative while charging. This source normalizes all of
// them: power is the absolute flow through the battery in watts (0 on AC
// with a full battery — the machine is then fed by the adapter, which is
// what the RAPL source measures), and charge falls back to
// charge_now/charge_full or energy_now/energy_full when there is no
// capacity file.
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
		isBattery := readString(filepath.Join(dir, "type")) == "Battery"

		add := func(label string, kind Kind, v float64) {
			out = append(out, Reading{Chip: chip, Label: label, Kind: kind, Value: v})
		}

		// Charge percentage, derived when the driver has no capacity file.
		if v, ok := readFloat(filepath.Join(dir, "capacity")); ok {
			add("charge", KindCapacity, v)
		} else if pct, ok := ratio(dir, "charge_now", "charge_full"); ok {
			add("charge", KindCapacity, pct)
		} else if pct, ok := ratio(dir, "energy_now", "energy_full"); ok {
			add("charge", KindCapacity, pct)
		}

		if v, ok := readFloat(filepath.Join(dir, "temp")); ok {
			add("temp", KindTemp, v/10) // tenths of °C
		}

		volt, hasVolt := readFloat(filepath.Join(dir, "voltage_now")) // µV
		curr, hasCurr := readFloat(filepath.Join(dir, "current_now")) // µA, negative = charging on some drivers
		power, hasPower := readFloat(filepath.Join(dir, "power_now")) // µW
		if !hasPower && hasVolt && hasCurr {
			// Charge-based gauges: derive µW from µV × µA.
			power, hasPower = volt*curr/1e6, true
		}

		// Idle USB-PD ports and adapters report all-zero electrics —
		// pure noise, unlike a battery where 0 W is meaningful.
		if !isBattery && volt == 0 && curr == 0 && power == 0 {
			continue
		}

		if hasVolt {
			add("voltage", KindVoltage, volt/1e6)
		}
		if hasCurr {
			add("current", KindCurrent, math.Abs(curr)/1e6)
		}
		if hasPower {
			add("power", KindPower, math.Abs(power)/1e6)
		}
	}
	return out
}

// ratio returns now/full as a percentage when both attributes exist.
func ratio(dir, nowFile, fullFile string) (float64, bool) {
	now, ok1 := readFloat(filepath.Join(dir, nowFile))
	full, ok2 := readFloat(filepath.Join(dir, fullFile))
	if !ok1 || !ok2 || full <= 0 {
		return 0, false
	}
	return now / full * 100, true
}
