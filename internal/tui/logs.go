package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/guptarohit/asciigraph"
)

// The logs tab has two sub-pages: a filterable record browser and a
// chart builder that graphs any time range from SQLite and overlays
// multiple series for comparison.
const (
	logsPageRecords = iota
	logsPageCharts
	numLogsPages
)

var logsPageNames = [numLogsPages]string{"records", "charts"}

// chartRanges are the selectable time windows; 0 = everything stored.
var chartRanges = []struct {
	label string
	d     time.Duration
}{
	{"5m", 5 * time.Minute},
	{"15m", 15 * time.Minute},
	{"1h", time.Hour},
	{"6h", 6 * time.Hour},
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"all", 0},
}

// seriesPalette colors compared series; legend styles must match.
var seriesPalette = []asciigraph.AnsiColor{205, 39, 42, 214, 51, 135}

func seriesStyle(i int) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(fmt.Sprint(int(seriesPalette[i%len(seriesPalette)]))))
}

// chartSpec is one requested series, parsed from the input line.
type chartSpec struct {
	kind string // system | proccpu | procmem | sensor
	arg  string // metric name, process name, or sensor key
	unit string
}

func (c chartSpec) String() string {
	switch c.kind {
	case "system":
		return c.arg
	case "proccpu":
		return c.arg + " cpu"
	case "procmem":
		return c.arg + " mem"
	default:
		return c.arg
	}
}

// parseChartSpecs turns "cpu, mem, firefox, mem:firefox, sensor:coretemp"
// into concrete series requests.
func parseChartSpecs(input string) []chartSpec {
	var out []chartSpec
	for _, tok := range strings.Split(input, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		low := strings.ToLower(tok)
		switch {
		case low == "cpu" || low == "mem":
			out = append(out, chartSpec{kind: "system", arg: low, unit: "%"})
		case low == "temp":
			out = append(out, chartSpec{kind: "system", arg: "temp", unit: "°C"})
		case strings.HasPrefix(low, "sensor:"):
			out = append(out, chartSpec{kind: "sensor", arg: strings.TrimSpace(tok[7:])})
		case strings.HasPrefix(low, "mem:"):
			out = append(out, chartSpec{kind: "procmem", arg: strings.TrimSpace(tok[4:]), unit: "MB"})
		case strings.HasPrefix(low, "proc:"):
			out = append(out, chartSpec{kind: "proccpu", arg: strings.TrimSpace(tok[5:]), unit: "%"})
		default:
			out = append(out, chartSpec{kind: "proccpu", arg: tok, unit: "%"})
		}
		if len(out) == len(seriesPalette) {
			break
		}
	}
	return out
}

// chartSeries is a fetched series ready to draw.
type chartSeries struct {
	spec chartSpec
	data []float64
}

type chartMsg struct {
	series []chartSeries
	err    error
}

// runChart queries every requested series from SQLite in one command.
func (m Model) runChart() tea.Cmd {
	if m.st == nil {
		return func() tea.Msg { return chartMsg{err: fmt.Errorf("no database available")} }
	}
	st := m.st
	specs := parseChartSpecs(m.chartInput.Value())
	since := chartRanges[m.chartRange].d
	buckets := m.graphWidth()
	return func() tea.Msg {
		var out []chartSeries
		for _, sp := range specs {
			var data []float64
			var err error
			switch sp.kind {
			case "system":
				data, err = st.SystemSeries(sp.arg, since, buckets)
			case "proccpu":
				data, err = st.ProcSeries(sp.arg, "cpu", since, buckets)
			case "procmem":
				data, err = st.ProcSeries(sp.arg, "mem", since, buckets)
			case "sensor":
				data, err = st.SensorSeries(sp.arg, since, buckets)
			}
			if err != nil {
				return chartMsg{err: err}
			}
			out = append(out, chartSeries{spec: sp, data: data})
		}
		return chartMsg{series: out}
	}
}

// ── view ────────────────────────────────────────────────────────────────

func (m Model) logsView() string {
	out := m.logsSubTabs() + "\n"
	if m.logsPage == logsPageRecords {
		return out + m.recordsView()
	}
	return out + m.chartsView()
}

func (m Model) logsSubTabs() string {
	var parts []string
	for i, name := range logsPageNames {
		if i == m.logsPage {
			parts = append(parts, lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color(colAccent)).Render("▸ "+name))
		} else {
			parts = append(parts, dimStyle.Render("  "+name))
		}
	}
	return panel("logs — v/click: switch page", strings.Join(parts, "    "), m.width, colBorder)
}

func (m Model) logsSubTabZones() []chipZone {
	x := 2
	var zones []chipZone
	for i, name := range logsPageNames {
		w := lipgloss.Width("▸ " + name)
		zones = append(zones, chipZone{x0: x, x1: x + w, id: fmt.Sprint(i)})
		x += w + 4
	}
	return zones
}

func (m Model) recordsView() string {
	fcolor := colBorder
	if m.filterInput.Focused() {
		fcolor = colAccent
	}
	out := panel("filter — /: edit · enter: apply · r: refresh", m.filterInput.View(), m.width, fcolor) + "\n"

	if m.logsErr != "" {
		return out + panel("history", errTextStyle.Render("query error: "+m.logsErr), m.width, colErr)
	}
	tcolor := colBorder
	if !m.filterInput.Focused() {
		tcolor = colAccent
	}
	return out + panel(fmt.Sprintf("history (%d rows)", len(m.logsTable.Rows())), m.logsTable.View(), m.width, tcolor)
}

func (m Model) chartsView() string {
	// Controls: series input + range chips.
	icolor := colBorder
	if m.chartInput.Focused() {
		icolor = colAccent
	}
	var chips []string
	for i, r := range chartRanges {
		if i == m.chartRange {
			chips = append(chips, lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color(colAccent)).Render("["+r.label+"]"))
		} else {
			chips = append(chips, dimStyle.Render(" "+r.label+" "))
		}
	}
	norm := dimStyle.Render("normalize: off (x)")
	if m.chartNorm {
		norm = lipgloss.NewStyle().Foreground(lipgloss.Color(colOK)).Render("normalize: on (x)")
	}
	controls := m.chartInput.View() + "\n" +
		headerStyle.Render("range ") + strings.Join(chips, " ") + "   " + norm
	out := panel("series — /: edit · enter: plot · w: range · r: refresh", controls, m.width, icolor) + "\n"

	if m.chartErr != "" {
		return out + panel("chart", errTextStyle.Render("query error: "+m.chartErr), m.width, colErr)
	}
	if len(m.chartData) == 0 {
		return out + panel("chart", dimStyle.Render(
			`type series separated by commas, then enter — e.g. "cpu, mem, firefox, mem:chromium, sensor:coretemp"`),
			m.width, colBorder)
	}

	// Assemble plottable series and the legend.
	var plotted [][]float64
	var colors []asciigraph.AnsiColor
	var legend []string
	for i, cs := range m.chartData {
		data := cs.data
		label := cs.spec.String()
		if len(data) < 2 {
			legend = append(legend, seriesStyle(i).Render("── "+label)+dimStyle.Render("  no data in range"))
			continue
		}
		stats := fmt.Sprintf("  last %.1f · avg %.1f · max %.1f %s", data[len(data)-1], avg(data), max(data), cs.spec.unit)
		if m.chartNorm {
			if mx := max(data); mx > 0 {
				norm := make([]float64, len(data))
				for j, v := range data {
					norm[j] = v / mx * 100
				}
				data = norm
			}
		}
		plotted = append(plotted, data)
		colors = append(colors, seriesPalette[i%len(seriesPalette)])
		legend = append(legend, seriesStyle(i).Render("── "+label)+dimStyle.Render(stats))
	}

	title := fmt.Sprintf("chart — %s · %d series", chartRanges[m.chartRange].label, len(plotted))
	if m.chartNorm {
		title += " · normalized to %% of each max"
	}
	var content string
	if len(plotted) == 0 {
		content = dimStyle.Render("no data in this range")
	} else {
		content = asciigraph.PlotMany(plotted,
			asciigraph.Height(m.chartGraphH()),
			asciigraph.Width(m.graphWidth()),
			asciigraph.LowerBound(0),
			asciigraph.SeriesColors(colors...),
		)
	}
	return out + panel(title, content+"\n"+strings.Join(legend, "\n"), m.width, colAccent)
}

// chartGraphH sizes the chart to fill the space under the controls.
func (m Model) chartGraphH() int {
	h := m.height - headerH - 3 - 5 - 4 - len(m.chartData) - 2
	if h < 5 {
		h = 5
	}
	return h
}

// ── input ───────────────────────────────────────────────────────────────

func (m Model) handleLogsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	key := msg.String()

	if m.chartInput.Focused() {
		switch key {
		case "ctrl+c":
			return m, tea.Quit, true
		case "esc":
			m.chartInput.Blur()
			return m, nil, true
		case "enter":
			m.chartInput.Blur()
			return m, m.runChart(), true
		}
		var cmd tea.Cmd
		m.chartInput, cmd = m.chartInput.Update(msg)
		return m, cmd, true
	}

	switch key {
	case "v":
		m.logsPage = (m.logsPage + 1) % numLogsPages
		if m.logsPage == logsPageCharts && len(m.chartData) == 0 {
			return m, m.runChart(), true
		}
		return m, nil, true
	case "/":
		if m.logsPage == logsPageCharts {
			m.chartInput.Focus()
			return m, nil, true
		}
	case "w":
		if m.logsPage == logsPageCharts {
			m.chartRange = (m.chartRange + 1) % len(chartRanges)
			return m, m.runChart(), true
		}
	case "x":
		if m.logsPage == logsPageCharts {
			m.chartNorm = !m.chartNorm
			return m, nil, true
		}
	case "r":
		if m.logsPage == logsPageCharts {
			return m, m.runChart(), true
		}
	}
	return m, nil, false // not handled here
}

func (m Model) handleLogsClick(x, y int) (tea.Model, tea.Cmd) {
	// Sub-page tabs (content row of the subtab panel).
	if y >= headerH && y < headerH+3 {
		for _, z := range m.logsSubTabZones() {
			if x >= z.x0 && x < z.x1 {
				page := int(z.id[0] - '0')
				if page != m.logsPage {
					m.logsPage = page
					if page == logsPageCharts && len(m.chartData) == 0 {
						return m, m.runChart()
					}
				}
				return m, nil
			}
		}
		return m, nil
	}

	if m.logsPage == logsPageCharts {
		// Series input line.
		if y == headerH+4 {
			m.chartInput.Focus()
			return m, nil
		}
		// Range chips line.
		if y == headerH+5 {
			cx := 2 + lipgloss.Width("range ")
			for i, r := range chartRanges {
				w := lipgloss.Width(r.label) + 2
				if x >= cx && x < cx+w {
					m.chartRange = i
					return m, m.runChart()
				}
				cx += w + 1
			}
		}
		if m.chartInput.Focused() {
			m.chartInput.Blur()
		}
		return m, nil
	}

	// Records page: filter box then table rows.
	if y >= headerH+3 && y < headerH+6 {
		m.filterInput.Focus()
		return m, textinput.Blink
	}
	if m.filterInput.Focused() {
		m.filterInput.Blur()
	}
	top := m.tableTop()
	t := &m.logsTable
	if y >= top && y < top+t.Height() && len(t.Rows()) > 0 {
		idx := visOffset(t.Cursor(), t.Height(), len(t.Rows())) + (y - top)
		if idx >= len(t.Rows()) {
			idx = len(t.Rows()) - 1
		}
		t.SetCursor(idx)
	}
	return m, nil
}
