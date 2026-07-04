package sensors

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHwmonRead(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		// AMD CPU with labelled temp + power
		"hwmon0/name":         "k10temp",
		"hwmon0/temp1_input":  "54250",
		"hwmon0/temp1_label":  "Tctl",
		"hwmon0/power1_input": "45000000",
		// NVMe drive with model
		"hwmon1/name":         "nvme",
		"hwmon1/device/model": "Samsung SSD 980",
		"hwmon1/temp1_input":  "38500",
		// EC fan + voltage; intrusion0_input must NOT match the "in" prefix
		"hwmon2/name":             "ec",
		"hwmon2/fan1_input":       "2400",
		"hwmon2/in0_input":        "12100",
		"hwmon2/intrusion0_input": "1",
	})

	got := (&Hwmon{root: root}).Read()

	want := map[string]struct {
		kind  Kind
		value float64
	}{
		"k10temp/Tctl":                {KindTemp, 54.25},
		"k10temp/power1":              {KindPower, 45},
		"nvme: Samsung SSD 980/temp1": {KindTemp, 38.5},
		"ec/fan1":                     {KindFan, 2400},
		"ec/in0":                      {KindVoltage, 12.1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d readings, want %d: %+v", len(got), len(want), got)
	}
	for _, r := range got {
		w, ok := want[r.Key()]
		if !ok {
			t.Errorf("unexpected reading %q", r.Key())
			continue
		}
		if r.Kind != w.kind || r.Value != w.value {
			t.Errorf("%s: got (%s, %v), want (%s, %v)", r.Key(), r.Kind, r.Value, w.kind, w.value)
		}
	}
}

func TestThermalSkipsHwmonBackedZones(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"thermal_zone0/type": "cpu-thermal",
		"thermal_zone0/temp": "48000",
		// This zone is also registered in hwmon — must be skipped.
		"thermal_zone1/type":        "acpitz",
		"thermal_zone1/temp":        "20000",
		"thermal_zone1/hwmon3/name": "acpitz",
	})

	got := (&Thermal{root: root}).Read()
	if len(got) != 1 {
		t.Fatalf("got %d readings, want 1: %+v", len(got), got)
	}
	if got[0].Label != "cpu-thermal" || got[0].Value != 48 {
		t.Errorf("got %+v, want cpu-thermal 48°C", got[0])
	}
}
