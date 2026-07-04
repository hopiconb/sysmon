package sensors

import (
	"os"
	"path/filepath"
	"strings"
)

// IIO reads /sys/bus/iio/devices — the Industrial I/O subsystem, where
// environmental and motion sensors land: ambient temperature, humidity,
// barometric pressure, light. Common on tablets, ARM boards, and USB
// sensor dongles with iio drivers.
type IIO struct {
	root string // test override
}

// iioChannels maps channel name -> kind and a conversion from the
// subsystem's canonical unit to ours.
var iioChannels = []struct {
	name string
	kind Kind
	conv func(float64) float64
}{
	{"in_temp_input", KindTemp, func(v float64) float64 { return v / 1000 }},                 // milli°C
	{"in_humidityrelative_input", KindHumidity, func(v float64) float64 { return v / 1000 }}, // m%RH
	{"in_pressure_input", KindPressure, func(v float64) float64 { return v * 10 }},           // kPa -> hPa
	{"in_illuminance_input", KindLight, func(v float64) float64 { return v }},                // lux
}

func (s *IIO) Read() []Reading {
	root := s.root
	if root == "" {
		root = "/sys/bus/iio/devices"
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Reading
	for _, d := range dirs {
		if !strings.HasPrefix(d.Name(), "iio:device") {
			continue
		}
		dir := filepath.Join(root, d.Name())
		name := readString(filepath.Join(dir, "name"))
		if name == "" {
			name = d.Name()
		}
		chip := "iio/" + name
		for _, ch := range iioChannels {
			if v, ok := readFloat(filepath.Join(dir, ch.name)); ok {
				label := strings.TrimSuffix(strings.TrimPrefix(ch.name, "in_"), "_input")
				out = append(out, Reading{Chip: chip, Label: label, Kind: ch.kind, Value: ch.conv(v)})
			}
		}
		// Raw+scale fallback for temp sensors without a processed channel.
		if raw, ok := readFloat(filepath.Join(dir, "in_temp_raw")); ok {
			if _, has := readFloat(filepath.Join(dir, "in_temp_input")); !has {
				scale, sok := readFloat(filepath.Join(dir, "in_temp_scale"))
				if !sok {
					scale = 1
				}
				offset, _ := readFloat(filepath.Join(dir, "in_temp_offset"))
				out = append(out, Reading{
					Chip: chip, Label: "temp", Kind: KindTemp,
					Value: (raw + offset) * scale / 1000,
				})
			}
		}
	}
	return out
}
