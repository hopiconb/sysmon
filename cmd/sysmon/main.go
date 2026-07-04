package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/hopiconb/sysmon/internal/collector"
	"github.com/hopiconb/sysmon/internal/config"
	"github.com/hopiconb/sysmon/internal/ipc"
	"github.com/hopiconb/sysmon/internal/store"
	"github.com/hopiconb/sysmon/internal/tui"
)

// version is stamped by the release build via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "daemon":
		err = runDaemon(os.Args[2:])
	case "tui":
		err = runTUI()
	case "tail":
		err = runTail()
	case "version", "--version", "-v":
		fmt.Println("sysmon", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("sysmon %s: %v", os.Args[1], err)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: sysmon <command>

  daemon [--detach]   run the collector (foreground; --detach forks into background)
  tui                 attach the interactive UI to a running daemon
  tail                print live samples as JSON lines`)
}

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	detach := fs.Bool("detach", false, "fork into the background, detached from the terminal")
	fs.Parse(args)

	if *detach {
		return detachSelf()
	}

	cfgPath := config.DefaultPath()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// Hot-reload: the sample loop reads the current config atomically each
	// tick, so focus-list edits apply without a restart.
	var current atomic.Pointer[config.Config]
	current.Store(&cfg)
	stopWatch, err := config.Watch(cfgPath, func(c config.Config) {
		current.Store(&c)
		log.Printf("config reloaded: focus=%v interval=%s", c.Focus, c.Interval())
	})
	if err != nil {
		log.Printf("config watch disabled: %v", err)
	} else {
		defer stopWatch()
	}

	dbPath := config.DefaultDBPath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	sockPath := ipc.SocketPath()
	srv, err := ipc.Listen(sockPath)
	if err != nil {
		return err
	}
	defer srv.Close()
	defer os.Remove(sockPath)

	log.Printf("sysmon daemon: db=%s socket=%s interval=%s focus=%v",
		dbPath, sockPath, cfg.Interval(), cfg.Focus)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	col := collector.New()
	// Prime CPU counters so the first real sample isn't zero.
	col.Collect(current.Load().Focus)

	interval := current.Load().Interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	prune := func() {
		if r := current.Load().Retention(); r > 0 {
			if err := st.Prune(r); err != nil {
				log.Printf("prune failed: %v", err)
			}
		}
	}
	prune()
	pruneTicker := time.NewTicker(time.Hour)
	defer pruneTicker.Stop()

	for {
		select {
		case <-sig:
			log.Println("sysmon daemon: shutting down")
			return nil
		case <-pruneTicker.C:
			prune()
		case <-ticker.C:
			c := current.Load()
			if iv := c.Interval(); iv != interval {
				interval = iv
				ticker.Reset(interval)
			}
			sm, err := col.Collect(c.Focus)
			if err != nil {
				log.Printf("sample failed: %v", err)
				continue
			}
			if err := st.Write(sm); err != nil {
				log.Printf("store write failed: %v", err)
			}
			srv.Broadcast(sm)
		}
	}
}

// detachSelf re-execs "sysmon daemon" in a new session (setsid) with stdio
// on /dev/null, fully detached from the controlling terminal — the ad-hoc
// alternative to running under systemd.
func detachSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()

	cmd := exec.Command(exe, "daemon")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Printf("sysmon daemon started in background (pid %d)\n", cmd.Process.Pid)
	return cmd.Process.Release()
}

func runTUI() error {
	stream, err := ipc.Dial(ipc.SocketPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon not running — showing stored history only")
		stream = nil
	}

	// The store is optional for the TUI: live streaming works without it,
	// only the logs tab and offline mode need it.
	var st *store.Store
	if s, err := store.Open(config.DefaultDBPath()); err == nil {
		st = s
		defer st.Close()
	}

	p := tea.NewProgram(tui.New(stream, st), tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func runTail() error {
	stream, err := ipc.Dial(ipc.SocketPath())
	if err != nil {
		return fmt.Errorf("cannot connect to daemon: %w (is sysmon.service running?)", err)
	}
	enc := json.NewEncoder(os.Stdout)
	for sm := range stream {
		if err := enc.Encode(sm); err != nil {
			return err
		}
	}
	return nil
}
