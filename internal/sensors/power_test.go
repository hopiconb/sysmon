package sensors

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPowerSupplyChargeBasedBattery(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		// Charge-based gauge, no capacity/power_now files, charging
		// (negative current) — common on older ACPI and ARM batteries.
		"BAT1/type":        "Battery",
		"BAT1/charge_now":  "2500000",
		"BAT1/charge_full": "5000000",
		"BAT1/voltage_now": "11400000", // 11.4 V
		"BAT1/current_now": "-2000000", // 2 A, charging
		// Idle USB-PD port: all-zero electrics must be dropped.
		"ucsi-source-psy-USBC000:001/type":        "USB",
		"ucsi-source-psy-USBC000:001/voltage_now": "0",
		"ucsi-source-psy-USBC000:001/current_now": "0",
	})

	got := (&PowerSupply{root: root}).Read()

	want := map[string]float64{
		"power/BAT1/charge":  50,
		"power/BAT1/voltage": 11.4,
		"power/BAT1/current": 2,
		"power/BAT1/power":   22.8, // |V × A|
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
		if math.Abs(r.Value-w) > 0.01 {
			t.Errorf("%s = %v, want %v", r.Key(), r.Value, w)
		}
	}
}

func TestRAPLWattsFromDelta(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"intel-rapl:0/name":                "package-0",
		"intel-rapl:0/energy_uj":           "1000000",
		"intel-rapl:0/max_energy_range_uj": "262143328850",
		// mmio duplicate of the package domain must be ignored.
		"intel-rapl-mmio:0/name":      "package-0",
		"intel-rapl-mmio:0/energy_uj": "1000000",
	})

	r := &RAPL{root: root}
	if got := r.Read(); len(got) != 0 {
		t.Fatalf("first read should yield no readings, got %+v", got)
	}

	time.Sleep(100 * time.Millisecond)
	// +1 J over ~0.1 s ≈ 10 W.
	if err := os.WriteFile(filepath.Join(root, "intel-rapl:0/energy_uj"), []byte("2000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := r.Read()
	if len(got) != 1 {
		t.Fatalf("got %d readings, want 1: %+v", len(got), got)
	}
	if got[0].Label != "package-0" || got[0].Kind != KindPower {
		t.Errorf("unexpected reading %+v", got[0])
	}
	if got[0].Value < 5 || got[0].Value > 20 {
		t.Errorf("watts = %v, want ~10", got[0].Value)
	}
}
