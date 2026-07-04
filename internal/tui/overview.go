package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/guptarohit/asciigraph"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"gopkg.in/yaml.v3"

	"github.com/hopiconb/sysmon/internal/collector"
	"github.com/hopiconb/sysmon/internal/config"
	"github.com/hopiconb/sysmon/internal/sensors"
)

// sections of the overview tab, each toggleable by key or click.
type section struct {
	id, label, key string
}

var overviewSections = []section{
	{"sys", "system", "i"},
	{"cpu", "cpu", "c"},
	{"mem", "memory", "m"},
	{"net", "network", "n"},
	{"disk", "disk", "d"},
	{"hw", "hardware", "h"},
	{"top", "top procs", "t"},
}

const (
	graphCPU = asciigraph.AnsiColor(205) // pink
	graphMem = asciigraph.AnsiColor(39)  // blue
	graphRx  = asciigraph.AnsiColor(42)  // green (download / read)
	graphTx  = asciigraph.AnsiColor(214) // orange (upload / write)
)

// ── preferences (persisted so the chosen layout survives restarts) ─────

type prefs struct {
	Show   map[string]bool `yaml:"show"`
	Window int             `yaml:"window"`
}

func prefsPath() string {
	return filepath.Join(filepath.Dir(config.DefaultPath()), "tui.yaml")
}

func loadPrefs() prefs {
	p := prefs{Show: map[string]bool{}, Window: histLen}
	for _, s := range overviewSections {
		p.Show[s.id] = true
	}
	data, err := os.ReadFile(prefsPath())
	if err != nil {
		return p
	}
	var saved prefs
	if yaml.Unmarshal(data, &saved) != nil {
		return p
	}
	for id, v := range saved.Show {
		p.Show[id] = v
	}
	if saved.Window >= 30 && saved.Window <= histLen {
		p.Window = saved.Window
	}
	return p
}

func (p prefs) save() {
	data, err := yaml.Marshal(p)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(prefsPath()), 0o755)
	_ = os.WriteFile(prefsPath(), data, 0o644)
}

// ── static system identity (read once; works offline too) ──────────────

func hostSummary() (string, string) {
	line1, line2 := "unknown host", ""
	if hi, err := host.Info(); err == nil {
		line1 = fmt.Sprintf("%s  ·  %s %s  ·  kernel %s",
			hi.Hostname, hi.Platform, hi.PlatformVersion, hi.KernelVersion)
	}
	if cis, err := cpu.Info(); err == nil && len(cis) > 0 {
		threads, _ := cpu.Counts(true)
		line2 = fmt.Sprintf("%s  ·  %d threads", strings.TrimSpace(cis[0].ModelName), threads)
	}
	return line1, line2
}

// ── the overview view ───────────────────────────────────────────────────

func (m Model) overviewView() string {
	var blocks []string
	blocks = append(blocks, m.viewChips())

	if m.show["sys"] {
		blocks = append(blocks, m.sysPanel(m.width))
	}

	wide := m.width >= 110
	gw, gh := m.graphWidth(), m.graphH()
	half := m.width / 2
	if wide {
		gw = half - 14
		if gw > m.window {
			gw = m.window
		}
	}

	cpuP, memP, netP, diskP := "", "", "", ""
	if m.show["cpu"] {
		cpuW := m.width
		if wide && m.show["mem"] {
			cpuW = half
		}
		cpuP = m.cpuPanel(cpuW, gw, gh)
	}
	if m.show["mem"] {
		memW := m.width
		if wide && m.show["cpu"] {
			memW = m.width - half
		}
		memP = m.memPanel(memW, gw, gh)
	}
	if m.show["net"] {
		netW := m.width
		if wide && m.show["disk"] {
			netW = half
		}
		netP = m.ratePanel("network", m.netRx, m.netTx, "↓", "↑", netW, gw, gh)
	}
	if m.show["disk"] {
		diskW := m.width
		if wide && m.show["net"] {
			diskW = m.width - half
		}
		diskP = m.ratePanel("disk i/o", m.diskR, m.diskW, "read", "write", diskW, gw, gh)
	}
	blocks = append(blocks, joinRow(wide, cpuP, memP)...)
	blocks = append(blocks, joinRow(wide, netP, diskP)...)

	if m.show["hw"] {
		blocks = append(blocks, m.hwPanel())
	}
	if m.show["top"] {
		blocks = append(blocks, m.topPanel())
	}
	if len(blocks) == 1 {
		blocks = append(blocks, dimStyle.Render("  everything hidden — toggle sections with the chips above"))
	}
	return strings.Join(blocks, "\n")
}

// joinRow places two panels side by side in wide mode, stacked otherwise.
func joinRow(wide bool, a, b string) []string {
	switch {
	case a == "" && b == "":
		return nil
	case a == "":
		return []string{b}
	case b == "":
		return []string{a}
	case wide:
		return []string{lipgloss.JoinHorizontal(lipgloss.Top, a, b)}
	default:
		return []string{a, b}
	}
}

// viewChips renders the toggle row; chipZones must mirror it exactly.
func (m Model) viewChips() string {
	var parts []string
	for _, s := range overviewSections {
		on := m.show[s.id]
		style := dimStyle
		mark := "○"
		if on {
			style = lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent))
			mark = "●"
		}
		parts = append(parts, style.Render(mark+" "+s.label+" ("+s.key+")"))
	}
	title := "view — click/keys: toggle · space: pause · +/-: zoom"
	if m.paused {
		title = "view — ⏸ PAUSED (space to resume)"
	}
	return panel(title, strings.Join(parts, "   "), m.width, colBorder)
}

type chipZone struct {
	x0, x1 int
	id     string
}

func (m Model) chipZones() []chipZone {
	x := 2 // panel left border + space
	var zones []chipZone
	for _, s := range overviewSections {
		w := lipgloss.Width("● " + s.label + " (" + s.key + ")")
		zones = append(zones, chipZone{x0: x, x1: x + w, id: s.id})
		x += w + 3 // separator
	}
	return zones
}

func (m Model) sysPanel(width int) string {
	up := "—"
	if m.latest.UptimeSec > 0 {
		up = humanDur(m.latest.UptimeSec)
	}
	dyn := fmt.Sprintf("up %s  ·  load %.2f %.2f %.2f  ·  %d processes",
		up, m.latest.Load1, m.latest.Load5, m.latest.Load15, m.latest.NumProcs)
	content := m.hostLine1 + "\n" + m.hostLine2 + "\n" + dyn
	return panel("system", content, width, colBorder)
}

func (m Model) cpuPanel(width, gw, gh int) string {
	view := tail(m.cpuHist, m.window)
	title := "cpu"
	if len(view) > 0 {
		title = fmt.Sprintf("cpu  %.1f%%  ·  avg %.1f  ·  max %.1f  ·  last %s",
			view[len(view)-1], avg(view), max(view), m.windowLabel())
	}
	content := plotColor(view, gw, gh, graphCPU)
	if len(m.latest.PerCore) > 0 {
		content += "\n" + headerStyle.Render("cores ") + coreBars(m.latest.PerCore)
	}
	return panel(title, content, width, colBorder)
}

func (m Model) memPanel(width, gw, gh int) string {
	view := tail(m.memHist, m.window)
	title := "memory"
	if len(view) > 0 {
		title = fmt.Sprintf("memory  %.1f%%  ·  %s / %s", view[len(view)-1],
			humanMB(m.latest.MemUsedMB), humanMB(m.latest.MemTotalMB))
		if m.latest.SwapUsedMB > 0.5 {
			title += fmt.Sprintf("  ·  swap %s", humanMB(m.latest.SwapUsedMB))
		}
	}
	return panel(title, plotColor(view, gw, gh, graphMem), width, colBorder)
}

// ratePanel graphs a pair of KB/s series (net rx/tx, disk read/write).
func (m Model) ratePanel(name string, a, b []float64, la, lb string, width, gw, gh int) string {
	va, vb := tail(a, m.window), tail(b, m.window)
	title := name
	if len(va) > 0 {
		title = fmt.Sprintf("%s  %s %s  ·  %s %s   (%s peak %s)",
			name, la, humanKBs(va[len(va)-1]), lb, humanKBs(vb[len(vb)-1]),
			la, humanKBs(max(va)))
	}
	var content string
	if len(va) < 2 {
		content = dimStyle.Render("waiting for data…")
	} else {
		content = asciigraph.PlotMany([][]float64{va, vb},
			asciigraph.Height(gh), asciigraph.Width(gw),
			asciigraph.LowerBound(0),
			asciigraph.SeriesColors(graphRx, graphTx),
		)
	}
	legend := lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("── "+la) + "  " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("── "+lb)
	return panel(title, content+"\n"+legend, width, colBorder)
}

func (m Model) hwPanel() string {
	var hw []string
	if line := kindLine(m.latest.Sensors, sensors.KindTemp, 6, m.width); line != "" {
		hw = append(hw, headerStyle.Render("temps ")+line)
	}
	if line := gpuLine(m.latest.Sensors, m.width); line != "" {
		hw = append(hw, headerStyle.Render("gpu   ")+line)
	}
	if line := kindLine(m.latest.Sensors, sensors.KindPower, 4, m.width); line != "" {
		hw = append(hw, headerStyle.Render("power ")+line)
	}
	if line := kindLine(m.latest.Sensors, sensors.KindFan, 4, m.width); line != "" {
		hw = append(hw, headerStyle.Render("fans  ")+line)
	}
	if len(hw) == 0 {
		hw = append(hw, dimStyle.Render("no sensor data"))
	}
	return panel("hardware", strings.Join(hw, "\n"), m.width, colBorder)
}

// topPanel is a compact top-5-by-CPU list with usage bars.
func (m Model) topPanel() string {
	// In focus mode the daemon sends procs in pid order — sort here.
	procs := make([]collector.ProcSample, len(m.latest.Procs))
	copy(procs, m.latest.Procs)
	sort.Slice(procs, func(i, j int) bool { return procs[i].CPUPct > procs[j].CPUPct })
	if len(procs) == 0 {
		return panel("top processes", dimStyle.Render("no process data"), m.width, colBorder)
	}
	n := 5
	if n > len(procs) {
		n = len(procs)
	}
	nameW := 24
	var lines []string
	for _, p := range procs[:n] {
		name := p.Name
		if len(name) > nameW {
			name = name[:nameW-1] + "…"
		}
		filled := int(p.CPUPct / 100 * 20)
		if filled > 20 {
			filled = 20
		}
		bar := lipgloss.NewStyle().Foreground(lipgloss.Color(colAccent)).Render(strings.Repeat("█", filled)) +
			dimStyle.Render(strings.Repeat("░", 20-filled))
		lines = append(lines, fmt.Sprintf("%-*s %6.1f%%  %s  %s",
			nameW, name, p.CPUPct, bar, humanMB(p.MemMB)))
	}
	return panel(fmt.Sprintf("top processes (%d tracked)", len(procs)), strings.Join(lines, "\n"), m.width, colBorder)
}

// ── helpers ─────────────────────────────────────────────────────────────

func tail(h []float64, n int) []float64 {
	if len(h) > n {
		return h[len(h)-n:]
	}
	return h
}

func avg(h []float64) float64 {
	if len(h) == 0 {
		return 0
	}
	var s float64
	for _, v := range h {
		s += v
	}
	return s / float64(len(h))
}

func max(h []float64) float64 {
	var m float64
	for _, v := range h {
		if v > m {
			m = v
		}
	}
	return m
}

func (m Model) windowLabel() string {
	secs := float64(m.window) * m.sampleInterval()
	if secs >= 120 {
		return fmt.Sprintf("%.0fm", secs/60)
	}
	return fmt.Sprintf("%.0fs", secs)
}

func humanMB(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024)
	}
	return fmt.Sprintf("%.0f MB", mb)
}

func humanKBs(kbs float64) string {
	switch {
	case kbs >= 1024*1024:
		return fmt.Sprintf("%.1f GB/s", kbs/1024/1024)
	case kbs >= 1024:
		return fmt.Sprintf("%.1f MB/s", kbs/1024)
	default:
		return fmt.Sprintf("%.0f KB/s", kbs)
	}
}

func humanDur(secs uint64) string {
	d := secs / 86400
	h := secs % 86400 / 3600
	mi := secs % 3600 / 60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, mi)
	default:
		return fmt.Sprintf("%dm", mi)
	}
}

func plotColor(data []float64, width, height int, color asciigraph.AnsiColor) string {
	if len(data) < 2 {
		return dimStyle.Render("waiting for data…")
	}
	return asciigraph.Plot(data,
		asciigraph.Height(height),
		asciigraph.Width(width),
		asciigraph.LowerBound(0),
		asciigraph.SeriesColors(color),
	)
}
