// Package config loads the YAML config and hot-reloads it on change.
package config

import (
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Focus            []string `yaml:"focus"`
	SampleIntervalMS int      `yaml:"sample_interval_ms"`
	RetentionDays    int      `yaml:"retention_days"` // 0 = default (7), -1 = keep forever
}

func (c Config) Retention() time.Duration {
	switch {
	case c.RetentionDays < 0:
		return 0 // keep forever
	case c.RetentionDays == 0:
		return 7 * 24 * time.Hour
	default:
		return time.Duration(c.RetentionDays) * 24 * time.Hour
	}
}

func (c Config) Interval() time.Duration {
	if c.SampleIntervalMS <= 0 {
		return time.Second
	}
	return time.Duration(c.SampleIntervalMS) * time.Millisecond
}

func DefaultPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "sysmon", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sysmon", "config.yaml")
}

// DefaultDBPath is where the daemon stores history.
func DefaultDBPath() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "sysmon", "sysmon.db")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "sysmon", "sysmon.db")
}

// Load reads the config file; a missing file yields defaults, not an error.
func Load(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	err = yaml.Unmarshal(data, &c)
	return c, err
}

// Watch calls onChange with the freshly loaded config whenever the file is
// written. Watching the directory (not the file) survives editors that
// replace the file via rename. Returns a stop function.
func Watch(path string, onChange func(Config)) (func(), error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.Close()
		return nil, err
	}
	if err := w.Add(dir); err != nil {
		w.Close()
		return nil, err
	}
	go func() {
		for ev := range w.Events {
			if ev.Name != path {
				continue
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			if c, err := Load(path); err == nil {
				onChange(c)
			}
		}
	}()
	return func() { w.Close() }, nil
}
