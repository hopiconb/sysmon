package tui

import (
	"testing"
	"time"
)

func TestParseFilter(t *testing.T) {
	f := parseFilter("firefox cpu>20 1h")
	if f.Name != "firefox" {
		t.Errorf("Name = %q, want firefox", f.Name)
	}
	if f.MinCPU != 20 {
		t.Errorf("MinCPU = %v, want 20", f.MinCPU)
	}
	if f.Since != time.Hour {
		t.Errorf("Since = %v, want 1h", f.Since)
	}

	f = parseFilter("gnome shell")
	if f.Name != "gnome shell" || f.MinCPU != 0 || f.Since != 0 {
		t.Errorf("plain words should only set Name, got %+v", f)
	}
}
