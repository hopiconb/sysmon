// Package tui is the Bubble Tea client. It attaches to a running daemon's
// socket for live samples, falling back to SQLite history when offline.
package tui

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/guptarohit/asciigraph"

	"github.com/hopiconb/sysmon/internal/collector"
	"github.com/hopiconb/sysmon/internal/sensors"
	"github.com/hopiconb/sysmon/internal/store"
)

const (
	tabOverview = iota
	tabFocus
	tabSensors
	tabLogs
	numTabs
)

const histLen = 120 // samples kept for graphs

var (
	tabStyle       = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color("245"))
	activeTabStyle = lipgloss.NewStyle().Padding(0, 2).Bold(true).Foreground(lipgloss.Color("212"))
	dimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	warnStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	headerStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
)

type sampleMsg collector.Sample
type streamClosedMsg struct{}
type logsResultMsg struct {
	rows []store.ProcRow
	err  error
}

type Model struct {
	width, height int
	tab           int
	live          bool

	stream <-chan collector.Sample
	st     *store.Store // nil if the DB couldn't be opened

	latest     collector.Sample
	cpuHist    []float64
	memHist    []float64
	procHist   map[string][]float64 // process name -> CPU history
	sensorHist map[string][]float64 // Reading.Key() -> value history

	procTable   table.Model
	sensorTable table.Model

	filterInput textinput.Model
	logsTable   table.Model
	logsErr     string
}

// New builds the model. stream may be nil (offline mode); st may be nil.
func New(stream <-chan collector.Sample, st *store.Store) Model {
	ti := textinput.New()
	ti.Placeholder = `filter: name, cpu>20, 15m  (e.g. "firefox cpu>10 1h")`
	ti.CharLimit = 80

	pt := table.New(table.WithColumns(procColumns(80)), table.WithHeight(10))
	snt := table.New(table.WithColumns(sensorColumns(80)), table.WithHeight(14))
	lt := table.New(table.WithColumns(logColumns(80)), table.WithHeight(15))

	m := Model{
		stream:      stream,
		st:          st,
		live:        stream != nil,
		procHist:    map[string][]float64{},
		sensorHist:  map[string][]float64{},
		procTable:   pt,
		sensorTable: snt,
		filterInput: ti,
		logsTable:   lt,
	}
	m.procTable.Focus()
	m.sensorTable.Focus()

	// Offline: preload graphs from history so the TUI is still useful.
	if !m.live && st != nil {
		if rows, err := st.RecentSystem(histLen); err == nil {
			for _, r := range rows {
				m.cpuHist = append(m.cpuHist, r.CPU)
				m.memHist = append(m.memHist, r.Mem)
			}
		}
	}
	return m
}

func (m Model) Init() tea.Cmd {
	return m.waitForSample()
}

func (m Model) waitForSample() tea.Cmd {
	if m.stream == nil {
		return nil
	}
	ch := m.stream
	return func() tea.Msg {
		sm, ok := <-ch
		if !ok {
			return streamClosedMsg{}
		}
		return sampleMsg(sm)
	}
}

func (m Model) queryLogs() tea.Cmd {
	if m.st == nil {
		return func() tea.Msg { return logsResultMsg{err: fmt.Errorf("no database available")} }
	}
	f := parseFilter(m.filterInput.Value())
	st := m.st
	return func() tea.Msg {
		rows, err := st.QueryProcs(f)
		return logsResultMsg{rows: rows, err: err}
	}
}

// parseFilter turns free text into a ProcFilter: "cpu>N" sets a CPU
// threshold, a duration token like "15m"/"2h" sets the time range, and
// anything else matches process names.
func parseFilter(s string) store.ProcFilter {
	f := store.ProcFilter{Limit: 500}
	var nameParts []string
	for _, tok := range strings.Fields(s) {
		low := strings.ToLower(tok)
		if v, ok := strings.CutPrefix(low, "cpu>"); ok {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				f.MinCPU = n
				continue
			}
		}
		if d, err := time.ParseDuration(low); err == nil && d > 0 {
			f.Since = d
			continue
		}
		nameParts = append(nameParts, tok)
	}
	f.Name = strings.Join(nameParts, " ")
	return f
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.procTable.SetColumns(procColumns(m.width))
		m.sensorTable.SetColumns(sensorColumns(m.width))
		m.logsTable.SetColumns(logColumns(m.width))
		th := m.height - 10
		if th < 3 {
			th = 3
		}
		m.logsTable.SetHeight(th)
		return m, nil

	case sampleMsg:
		m.applySample(collector.Sample(msg))
		return m, m.waitForSample()

	case streamClosedMsg:
		m.live = false
		return m, nil

	case logsResultMsg:
		if msg.err != nil {
			m.logsErr = msg.err.Error()
			return m, nil
		}
		m.logsErr = ""
		rows := make([]table.Row, 0, len(msg.rows))
		for _, r := range msg.rows {
			rows = append(rows, table.Row{
				r.TS.Format("15:04:05"),
				strconv.Itoa(int(r.PID)),
				r.Name,
				fmt.Sprintf("%.1f", r.CPU),
				fmt.Sprintf("%.0f", r.MemMB),
			})
		}
		m.logsTable.SetRows(rows)
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) applySample(sm collector.Sample) {
	m.latest = sm
	m.cpuHist = push(m.cpuHist, sm.CPUPct)
	m.memHist = push(m.memHist, sm.MemPct)

	seen := map[string]bool{}
	for _, p := range sm.Procs {
		if seen[p.Name] {
			continue // graph the first (hottest) instance per name
		}
		seen[p.Name] = true
		m.procHist[p.Name] = push(m.procHist[p.Name], p.CPUPct)
	}
	for name := range m.procHist {
		if !seen[name] {
			delete(m.procHist, name)
		}
	}

	sel := m.procTable.Cursor()
	rows := make([]table.Row, 0, len(sm.Procs))
	for _, p := range sm.Procs {
		rows = append(rows, table.Row{
			p.Name,
			strconv.Itoa(int(p.PID)),
			fmt.Sprintf("%.1f", p.CPUPct),
			fmt.Sprintf("%.0f", p.MemMB),
		})
	}
	m.procTable.SetRows(rows)
	if sel < len(rows) {
		m.procTable.SetCursor(sel)
	}

	seenSensor := map[string]bool{}
	sensorRows := make([]table.Row, 0, len(sm.Sensors))
	for _, r := range sm.Sensors {
		key := r.Key()
		seenSensor[key] = true
		m.sensorHist[key] = push(m.sensorHist[key], r.Value)
		sensorRows = append(sensorRows, table.Row{
			r.Chip,
			r.Label,
			string(r.Kind),
			fmt.Sprintf("%.1f %s", r.Value, r.Kind.Unit()),
		})
	}
	for key := range m.sensorHist {
		if !seenSensor[key] {
			delete(m.sensorHist, key) // device unplugged
		}
	}
	ssel := m.sensorTable.Cursor()
	m.sensorTable.SetRows(sensorRows)
	if ssel < len(sensorRows) {
		m.sensorTable.SetCursor(ssel)
	}
}

func push(hist []float64, v float64) []float64 {
	hist = append(hist, v)
	if len(hist) > histLen {
		hist = hist[len(hist)-histLen:]
	}
	return hist
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// While typing a filter, only intercept esc/enter/ctrl+c.
	if m.tab == tabLogs && m.filterInput.Focused() {
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "esc":
			m.filterInput.Blur()
			return m, nil
		case "enter":
			m.filterInput.Blur()
			return m, m.queryLogs()
		}
		var cmd tea.Cmd
		m.filterInput, cmd = m.filterInput.Update(msg)
		return m, cmd
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.tab = (m.tab + 1) % numTabs
		if m.tab == tabLogs {
			return m, m.queryLogs()
		}
		return m, nil
	case "shift+tab":
		m.tab = (m.tab - 1 + numTabs) % numTabs
		return m, nil
	case "/":
		if m.tab == tabLogs {
			m.filterInput.Focus()
			return m, textinput.Blink
		}
	case "r":
		if m.tab == tabLogs {
			return m, m.queryLogs()
		}
	}

	var cmd tea.Cmd
	switch m.tab {
	case tabFocus:
		m.procTable, cmd = m.procTable.Update(msg)
	case tabSensors:
		m.sensorTable, cmd = m.sensorTable.Update(msg)
	case tabLogs:
		m.logsTable, cmd = m.logsTable.Update(msg)
	}
	return m, cmd
}

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.tabBar())
	b.WriteString("\n\n")
	switch m.tab {
	case tabOverview:
		b.WriteString(m.overviewView())
	case tabFocus:
		b.WriteString(m.focusView())
	case tabSensors:
		b.WriteString(m.sensorsView())
	case tabLogs:
		b.WriteString(m.logsView())
	}
	return b.String()
}

func (m Model) tabBar() string {
	names := []string{"overview", "focus", "sensors", "logs"}
	parts := make([]string, numTabs)
	for i, n := range names {
		if i == m.tab {
			parts[i] = activeTabStyle.Render(n)
		} else {
			parts[i] = tabStyle.Render(n)
		}
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	status := dimStyle.Render("  live")
	if !m.live {
		status = warnStyle.Render("  offline — history only (systemctl --user start sysmon)")
	}
	return bar + status + dimStyle.Render("   tab: switch · q: quit")
}

func (m Model) graphWidth() int {
	w := m.width - 12
	if w < 20 {
		w = 20
	}
	if w > histLen {
		w = histLen
	}
	return w
}

func plot(data []float64, caption string, width int) string {
	if len(data) < 2 {
		return dimStyle.Render(caption + ": waiting for data…")
	}
	return asciigraph.Plot(data,
		asciigraph.Height(8),
		asciigraph.Width(width),
		asciigraph.Caption(caption),
		asciigraph.LowerBound(0),
	)
}

func (m Model) overviewView() string {
	var b strings.Builder
	w := m.graphWidth()

	cur := ""
	if len(m.cpuHist) > 0 {
		cur = fmt.Sprintf(" (now %.1f%%)", m.cpuHist[len(m.cpuHist)-1])
	}
	b.WriteString(plot(m.cpuHist, "CPU %"+cur, w))
	b.WriteString("\n\n")

	if len(m.latest.PerCore) > 0 {
		b.WriteString(headerStyle.Render("cores ") + coreBars(m.latest.PerCore) + "\n\n")
	}

	memCur := ""
	if len(m.memHist) > 0 {
		memCur = fmt.Sprintf(" (now %.1f%%, %.0f MB)", m.memHist[len(m.memHist)-1], m.latest.MemUsedMB)
	}
	b.WriteString(plot(m.memHist, "Mem %"+memCur, w))
	b.WriteString("\n\n")

	if line := kindLine(m.latest.Sensors, sensors.KindTemp, 6, m.width); line != "" {
		b.WriteString(headerStyle.Render("temps ") + line + "\n")
	}
	if line := gpuLine(m.latest.Sensors, m.width); line != "" {
		b.WriteString(headerStyle.Render("gpu   ") + line + "\n")
	}
	if line := kindLine(m.latest.Sensors, sensors.KindFan, 4, m.width); line != "" {
		b.WriteString(headerStyle.Render("fans  ") + line + "\n")
	}
	return b.String()
}

// coreBars renders per-core load as one compact block-character row —
// the resolution a per-core view actually needs, without per-core graphs.
func coreBars(cores []float64) string {
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, c := range cores {
		idx := int(c / 100 * float64(len(blocks)))
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		if idx < 0 {
			idx = 0
		}
		b.WriteRune(blocks[idx])
	}
	return b.String()
}

// kindLine summarizes the top-n hottest/highest readings of one kind.
func kindLine(rs []sensors.Reading, kind sensors.Kind, n, width int) string {
	var of []sensors.Reading
	for _, r := range rs {
		if r.Kind == kind && r.Value > 0 {
			of = append(of, r)
		}
	}
	if len(of) == 0 {
		return ""
	}
	sort.Slice(of, func(i, j int) bool { return of[i].Value > of[j].Value })
	if len(of) > n {
		of = of[:n]
	}
	var parts []string
	for _, r := range of {
		parts = append(parts, fmt.Sprintf("%s %s %.0f%s", r.Chip, r.Label, r.Value, kind.Unit()))
	}
	return clip(strings.Join(parts, "  ·  "), width)
}

// gpuLine shows utilization/VRAM for every detected GPU.
func gpuLine(rs []sensors.Reading, width int) string {
	var parts []string
	for _, r := range rs {
		if strings.HasPrefix(r.Chip, "gpu/") && (r.Label == "busy" || r.Label == "vram") {
			parts = append(parts, fmt.Sprintf("%s %s %.0f%%", strings.TrimPrefix(r.Chip, "gpu/"), r.Label, r.Value))
		}
	}
	return clip(strings.Join(parts, "  ·  "), width)
}

func clip(line string, width int) string {
	if width > 10 && len(line) > width-8 {
		return line[:width-8] + "…"
	}
	return line
}

func (m Model) focusView() string {
	var b strings.Builder
	b.WriteString(m.procTable.View())
	b.WriteString("\n\n")

	rows := m.procTable.Rows()
	if c := m.procTable.Cursor(); c >= 0 && c < len(rows) {
		name := rows[c][0]
		b.WriteString(plot(m.procHist[name], name+" CPU %", m.graphWidth()))
	} else {
		b.WriteString(dimStyle.Render("no tracked processes — add names to the focus list in config.yaml"))
	}
	return b.String()
}

func (m Model) sensorsView() string {
	var b strings.Builder
	b.WriteString(m.sensorTable.View())
	b.WriteString("\n\n")

	rows := m.sensorTable.Rows()
	if c := m.sensorTable.Cursor(); c >= 0 && c < len(rows) {
		key := rows[c][0] + "/" + rows[c][1]
		b.WriteString(plot(m.sensorHist[key], key, m.graphWidth()))
	} else {
		b.WriteString(dimStyle.Render("no sensors detected — see README for kernel modules (e.g. drivetemp for SATA drives)"))
	}
	return b.String()
}

func (m Model) logsView() string {
	var b strings.Builder
	b.WriteString(m.filterInput.View())
	b.WriteString(dimStyle.Render("   /: edit filter · enter: apply · r: refresh"))
	b.WriteString("\n\n")
	if m.logsErr != "" {
		b.WriteString(warnStyle.Render("query error: " + m.logsErr))
		return b.String()
	}
	b.WriteString(m.logsTable.View())
	return b.String()
}

func procColumns(width int) []table.Column {
	nameW := width - 40
	if nameW < 16 {
		nameW = 16
	}
	return []table.Column{
		{Title: "name", Width: nameW},
		{Title: "pid", Width: 8},
		{Title: "cpu %", Width: 8},
		{Title: "mem MB", Width: 10},
	}
}

func sensorColumns(width int) []table.Column {
	chipW := (width - 30) / 2
	if chipW < 18 {
		chipW = 18
	}
	return []table.Column{
		{Title: "device", Width: chipW},
		{Title: "sensor", Width: chipW},
		{Title: "kind", Width: 9},
		{Title: "value", Width: 14},
	}
}

func logColumns(width int) []table.Column {
	nameW := width - 48
	if nameW < 16 {
		nameW = 16
	}
	return []table.Column{
		{Title: "time", Width: 10},
		{Title: "pid", Width: 8},
		{Title: "name", Width: nameW},
		{Title: "cpu %", Width: 8},
		{Title: "mem MB", Width: 10},
	}
}
