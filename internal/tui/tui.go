// Package tui is the Bubble Tea client. It attaches to a running daemon's
// socket for live samples, falling back to SQLite history when offline.
// Every section is a bordered panel; tabs are clickable buttons; tables
// respond to the mouse wheel and clicks as well as the keyboard.
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

var tabNames = [numTabs]string{"overview", "focus", "sensors", "logs"}

const histLen = 120 // samples kept for graphs

const (
	colAccent = "212" // pink — active / selected
	colInfo   = "39"  // blue — headings, brand
	colDim    = "241"
	colBorder = "238"
	colOK     = "42"  // green
	colWarn   = "214" // orange
	colErr    = "196" // red
)

var (
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color(colDim))
	headerStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colInfo))
	logoWaveStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(colInfo))
	logoNameStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(colAccent))
	errTextStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(colErr))
)

// health is the tri-state indicator shown in the header.
type health int

const (
	healthOK   health = iota // everything working
	healthWarn               // degraded but functional
	healthErr                // something is broken
)

func (h health) color() string {
	switch h {
	case healthOK:
		return colOK
	case healthWarn:
		return colWarn
	}
	return colErr
}

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

func newTable(cols []table.Column, height int) table.Model {
	t := table.New(table.WithColumns(cols), table.WithHeight(height))
	st := table.DefaultStyles()
	st.Header = st.Header.Bold(true).Foreground(lipgloss.Color(colInfo))
	st.Selected = st.Selected.Bold(true).
		Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57"))
	t.SetStyles(st)
	t.Focus()
	return t
}

// New builds the model. stream may be nil (offline mode); st may be nil.
func New(stream <-chan collector.Sample, st *store.Store) Model {
	ti := textinput.New()
	ti.Placeholder = `name, cpu>20, 15m  (e.g. "firefox cpu>10 1h")`
	ti.CharLimit = 80

	m := Model{
		width:       80,
		height:      24,
		stream:      stream,
		st:          st,
		live:        stream != nil,
		procHist:    map[string][]float64{},
		sensorHist:  map[string][]float64{},
		procTable:   newTable(procColumns(72), 10),
		sensorTable: newTable(sensorColumns(72), 12),
		filterInput: ti,
		logsTable:   newTable(logColumns(72), 12),
	}

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

// health reports the tri-state status and its label for the header.
func (m Model) health() (health, string) {
	switch {
	case !m.live && m.st == nil:
		return healthErr, "no daemon, no database"
	case !m.live:
		return healthWarn, "offline — history only"
	case m.st == nil:
		return healthWarn, "live, logs unavailable"
	default:
		return healthOK, "live"
	}
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

// ── layout ──────────────────────────────────────────────────────────────

const headerH = 3 // bordered tab buttons

// tableTop returns the terminal row of the first data row of the active
// tab's table: header + panel border + table header row.
func (m Model) tableTop() int {
	if m.tab == tabLogs {
		return headerH + 3 + 2 // filter panel, then results panel chrome
	}
	return headerH + 2
}

func (m Model) graphH() int {
	h := (m.height - headerH - 15) / 2
	if h < 4 {
		h = 4
	}
	if h > 9 {
		h = 9
	}
	return h
}

func (m Model) tableH() int {
	h := m.height - headerH - (m.graphH() + 4) - 3
	if h < 4 {
		h = 4
	}
	return h
}

func (m Model) logsH() int {
	h := m.height - headerH - 3 - 3
	if h < 4 {
		h = 4
	}
	return h
}

func (m *Model) resize() {
	inner := m.width - 8
	m.procTable.SetColumns(procColumns(inner))
	m.sensorTable.SetColumns(sensorColumns(inner))
	m.logsTable.SetColumns(logColumns(inner))
	m.procTable.SetHeight(m.tableH())
	m.sensorTable.SetHeight(m.tableH())
	m.logsTable.SetHeight(m.logsH())
	m.filterInput.Width = m.width - 12
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
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

	case tea.MouseMsg:
		return m.handleMouse(msg)
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

// ── input: keyboard ─────────────────────────────────────────────────────

func (m Model) switchTab(tab int) (tea.Model, tea.Cmd) {
	m.tab = tab
	if tab == tabLogs {
		return m, m.queryLogs()
	}
	return m, nil
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
		return m.switchTab((m.tab + 1) % numTabs)
	case "shift+tab":
		return m.switchTab((m.tab - 1 + numTabs) % numTabs)
	case "1", "2", "3", "4":
		return m.switchTab(int(msg.String()[0] - '1'))
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
	return m.updateActiveTable(msg)
}

func (m Model) updateActiveTable(msg tea.Msg) (tea.Model, tea.Cmd) {
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

// ── input: mouse ────────────────────────────────────────────────────────

func (m Model) handleMouse(e tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch {
	case e.Button == tea.MouseButtonWheelUp:
		return m.updateActiveTable(tea.KeyMsg(tea.Key{Type: tea.KeyUp}))
	case e.Button == tea.MouseButtonWheelDown:
		return m.updateActiveTable(tea.KeyMsg(tea.Key{Type: tea.KeyDown}))
	case e.Action == tea.MouseActionPress && e.Button == tea.MouseButtonLeft:
		return m.handleClick(e.X, e.Y)
	}
	return m, nil
}

func (m Model) handleClick(x, y int) (tea.Model, tea.Cmd) {
	// Tab buttons in the header.
	if y < headerH {
		for _, z := range m.tabZones() {
			if x >= z.x0 && x < z.x1 {
				return m.switchTab(z.tab)
			}
		}
		return m, nil
	}

	// The filter box on the logs tab.
	if m.tab == tabLogs && y >= headerH && y < headerH+3 {
		m.filterInput.Focus()
		return m, textinput.Blink
	}
	if m.filterInput.Focused() {
		m.filterInput.Blur()
	}

	// Row selection in the active table.
	t := m.activeTable()
	if t == nil {
		return m, nil
	}
	top := m.tableTop()
	if y < top || y >= top+t.Height() {
		return m, nil
	}
	n := len(t.Rows())
	if n == 0 {
		return m, nil
	}
	idx := visOffset(t.Cursor(), t.Height(), n) + (y - top)
	if idx >= n {
		idx = n - 1
	}
	t.SetCursor(idx)
	return m, nil
}

func (m *Model) activeTable() *table.Model {
	switch m.tab {
	case tabFocus:
		return &m.procTable
	case tabSensors:
		return &m.sensorTable
	case tabLogs:
		return &m.logsTable
	}
	return nil
}

// visOffset estimates the index of the first visible row. The bubbles
// table keeps its scroll offset private; this assumes the cursor sits at
// the bottom edge once the list has scrolled, which holds for sequential
// wheel/key navigation.
func visOffset(cursor, height, n int) int {
	if n <= height {
		return 0
	}
	off := cursor - (height - 1)
	if off < 0 {
		off = 0
	}
	if off > n-height {
		off = n - height
	}
	return off
}

// ── view ────────────────────────────────────────────────────────────────

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(m.headerView())
	b.WriteString("\n")
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
	b.WriteString("\n")
	b.WriteString(dimStyle.Render(" tab/1-4/click: switch · wheel/↑↓: select · q: quit"))
	return b.String()
}

// button renders a 3-row bordered button.
func button(label string, active bool) string {
	s := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colBorder)).
		Foreground(lipgloss.Color(colDim)).
		Padding(0, 1)
	if active {
		s = s.BorderForeground(lipgloss.Color(colAccent)).
			Foreground(lipgloss.Color(colAccent)).Bold(true)
	}
	return s.Render(label)
}

func (m Model) brandBox() string {
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(colInfo)).
		Padding(0, 1).
		Render(logoWaveStyle.Render("▁▂▄█") + logoNameStyle.Render(" sysmon"))
}

func (m Model) statusBox() string {
	h, label := m.health()
	c := lipgloss.Color(h.color())
	dot := lipgloss.NewStyle().Foreground(c).Render("●")
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(c).
		Padding(0, 1).
		Render(dot + " " + lipgloss.NewStyle().Foreground(c).Render(label))
}

type tabZone struct{ x0, x1, tab int }

// tabZones computes the clickable x-range of each tab button. Must mirror
// headerView's composition exactly.
func (m Model) tabZones() []tabZone {
	x := lipgloss.Width(m.brandBox())
	zones := make([]tabZone, 0, numTabs)
	for i, name := range tabNames {
		w := lipgloss.Width(button(name, i == m.tab))
		zones = append(zones, tabZone{x0: x, x1: x + w, tab: i})
		x += w
	}
	return zones
}

func (m Model) headerView() string {
	parts := []string{m.brandBox()}
	for i, name := range tabNames {
		parts = append(parts, button(name, i == m.tab))
	}
	left := lipgloss.JoinHorizontal(lipgloss.Top, parts...)
	status := m.statusBox()
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(status)
	if gap < 1 {
		return left
	}
	spacer := lipgloss.NewStyle().Width(gap).Render("")
	return lipgloss.JoinHorizontal(lipgloss.Top, left, spacer, status)
}

// panel draws a bordered box with the title embedded in the top border.
func panel(title, content string, width int, color string) string {
	if width < 12 {
		width = 12
	}
	inner := width - 4
	bs := lipgloss.NewStyle().Foreground(lipgloss.Color(color))
	ts := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(color))
	if lipgloss.Width(title) > inner-1 {
		title = title[:inner-1]
	}
	tw := lipgloss.Width(title)

	trunc := lipgloss.NewStyle().MaxWidth(inner)
	var b strings.Builder
	b.WriteString(bs.Render("╭─ ") + ts.Render(title) +
		bs.Render(" "+strings.Repeat("─", inner-tw-1)+"╮") + "\n")
	for _, line := range strings.Split(content, "\n") {
		line = trunc.Render(line)
		pad := inner - lipgloss.Width(line)
		if pad < 0 {
			pad = 0
		}
		b.WriteString(bs.Render("│ ") + line + strings.Repeat(" ", pad) + bs.Render(" │") + "\n")
	}
	b.WriteString(bs.Render("╰" + strings.Repeat("─", width-2) + "╯"))
	return b.String()
}

func (m Model) graphWidth() int {
	w := m.width - 14 // panel chrome + y-axis labels
	if w < 20 {
		w = 20
	}
	if w > histLen {
		w = histLen
	}
	return w
}

func plot(data []float64, width, height int) string {
	if len(data) < 2 {
		return dimStyle.Render("waiting for data…")
	}
	return asciigraph.Plot(data,
		asciigraph.Height(height),
		asciigraph.Width(width),
		asciigraph.LowerBound(0),
	)
}

func (m Model) overviewView() string {
	gw, gh := m.graphWidth(), m.graphH()

	cpuTitle := "CPU"
	if len(m.cpuHist) > 0 {
		cpuTitle = fmt.Sprintf("CPU  %.1f%%", m.cpuHist[len(m.cpuHist)-1])
	}
	cpuContent := plot(m.cpuHist, gw, gh)
	if len(m.latest.PerCore) > 0 {
		cpuContent += "\n" + headerStyle.Render("cores ") + coreBars(m.latest.PerCore)
	}

	memTitle := "Memory"
	if len(m.memHist) > 0 {
		memTitle = fmt.Sprintf("Memory  %.1f%%  ·  %.0f MB", m.memHist[len(m.memHist)-1], m.latest.MemUsedMB)
	}

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

	return panel(cpuTitle, cpuContent, m.width, colBorder) + "\n" +
		panel(memTitle, plot(m.memHist, gw, gh), m.width, colBorder) + "\n" +
		panel("hardware", strings.Join(hw, "\n"), m.width, colBorder)
}

// coreBars renders per-core load as one compact block-character row.
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
	if width > 14 && lipgloss.Width(line) > width-12 {
		return lipgloss.NewStyle().MaxWidth(width-12).Render(line) + "…"
	}
	return line
}

func (m Model) focusView() string {
	out := panel("processes", m.procTable.View(), m.width, colAccent) + "\n"

	rows := m.procTable.Rows()
	if c := m.procTable.Cursor(); c >= 0 && c < len(rows) {
		name := rows[c][0]
		out += panel(name+" · CPU %", plot(m.procHist[name], m.graphWidth(), m.graphH()), m.width, colBorder)
	} else {
		out += panel("graph", dimStyle.Render("no tracked processes — add names to the focus list in config.yaml"), m.width, colBorder)
	}
	return out
}

func (m Model) sensorsView() string {
	out := panel("sensors", m.sensorTable.View(), m.width, colAccent) + "\n"

	rows := m.sensorTable.Rows()
	if c := m.sensorTable.Cursor(); c >= 0 && c < len(rows) {
		key := rows[c][0] + "/" + rows[c][1]
		out += panel(key, plot(m.sensorHist[key], m.graphWidth(), m.graphH()), m.width, colBorder)
	} else {
		out += panel("graph", dimStyle.Render("no sensors detected — see README for kernel modules (e.g. drivetemp)"), m.width, colBorder)
	}
	return out
}

func (m Model) logsView() string {
	fcolor := colBorder
	if m.filterInput.Focused() {
		fcolor = colAccent
	}
	out := panel("filter — /: edit · enter: apply · r: refresh", m.filterInput.View(), m.width, fcolor) + "\n"

	if m.logsErr != "" {
		out += panel("history", errTextStyle.Render("query error: "+m.logsErr), m.width, colErr)
		return out
	}
	tcolor := colBorder
	if !m.filterInput.Focused() {
		tcolor = colAccent
	}
	out += panel("history", m.logsTable.View(), m.width, tcolor)
	return out
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
	chipW := (width - 33) / 2
	if chipW < 16 {
		chipW = 16
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
