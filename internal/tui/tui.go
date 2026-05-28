// Package tui is the Bubbletea TUI for kubectl-gpugo. It owns no scrape
// logic of its own — it just kicks scrape commands on a 2s ticker and renders
// the most recent snapshot. Scrapes run as tea.Cmd goroutines so a slow
// apiserver round-trip never blocks input.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/scraper"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	tickInterval  = 2 * time.Second
	scrapeTimeout = 10 * time.Second

	// reservedRows is what the View() chrome occupies outside the scrollable
	// body: title (1) + blank (1) + column header (1) + bottom hint (1).
	// Errors / fallback hints add more lines dynamically.
	reservedRows = 4
)

type tickMsg time.Time

type scrapeMsg struct {
	pods []scraper.PodGPU
	err  error
	at   time.Time
}

type Model struct {
	scr          *scraper.Scraper
	pods         []scraper.PodGPU
	lastErr      error
	lastScrape   time.Time
	scraping     bool // true while a scrape is in flight; prevents pile-up
	width        int
	height       int
	scrollOffset int
}

func NewModel(s *scraper.Scraper) Model {
	return Model{scr: s}
}

func (m Model) Init() tea.Cmd {
	m.scraping = true
	return tea.Batch(scrapeCmd(m.scr), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func scrapeCmd(s *scraper.Scraper) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), scrapeTimeout)
		defer cancel()
		p, err := s.Snapshot(ctx)
		return scrapeMsg{pods: p, err: err, at: time.Now()}
	}
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			if m.scraping {
				return m, nil
			}
			m.scraping = true
			return m, scrapeCmd(m.scr)
		case "up", "k":
			if m.scrollOffset > 0 {
				m.scrollOffset--
			}
		case "down", "j":
			if m.scrollOffset < m.maxScroll() {
				m.scrollOffset++
			}
		case "pgup", "b":
			m.scrollOffset = max(0, m.scrollOffset-m.bodyHeight())
		case "pgdown", " ", "f":
			m.scrollOffset = min(m.maxScroll(), m.scrollOffset+m.bodyHeight())
		case "home", "g":
			m.scrollOffset = 0
		case "end", "G":
			m.scrollOffset = m.maxScroll()
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		if m.scrollOffset > m.maxScroll() {
			m.scrollOffset = m.maxScroll()
		}
	case tickMsg:
		cmds := []tea.Cmd{tickCmd()}
		if !m.scraping {
			m.scraping = true
			cmds = append(cmds, scrapeCmd(m.scr))
		}
		return m, tea.Batch(cmds...)
	case scrapeMsg:
		m.scraping = false
		m.lastErr = msg.err
		m.lastScrape = msg.at
		if msg.err == nil {
			m.pods = msg.pods
		}
		if m.scrollOffset > m.maxScroll() {
			m.scrollOffset = m.maxScroll()
		}
	}
	return m, nil
}

var (
	styleTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	styleColHdr = lipgloss.NewStyle().Foreground(lipgloss.Color("99")).Bold(true)
	styleDim    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleYellow = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	styleRed    = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	styleErr    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleHint   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

func utilStyle(pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return styleRed
	case pct >= 70:
		return styleYellow
	case pct >= 30:
		return styleGreen
	default:
		return styleDim
	}
}

var (
	tableCols   = []string{"NAMESPACE", "POD", "NODE", "GPU", "GPU%", "VRAM USED", "POWER"}
	tableWidths = []int{16, 48, 14, 5, 7, 22, 8}
)

// sortedPods returns m.pods sorted for display: by first GPU index (so rows
// on the same physical card / MIG slice group together), then by VRAM-used
// desc within a GPU, then alphabetically.
func (m Model) sortedPods() []scraper.PodGPU {
	pods := append([]scraper.PodGPU(nil), m.pods...)
	sort.Slice(pods, func(i, j int) bool {
		gi, gj := gpuKey(pods[i]), gpuKey(pods[j])
		if gi != gj {
			return gi < gj
		}
		if pods[i].VRAMUsedMiB != pods[j].VRAMUsedMiB {
			return pods[i].VRAMUsedMiB > pods[j].VRAMUsedMiB
		}
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Pod < pods[j].Pod
	})
	return pods
}

// renderBodyLines produces the scrollable body as a slice of pre-rendered
// strings (one row each, with blank separators between GPU groups). Returns
// the lines and a flag indicating whether any row is a per-GPU fallback row
// (used to decide whether to print the "enable --kubernetes" hint).
func (m Model) renderBodyLines() (lines []string, fallback bool) {
	pods := m.sortedPods()
	prevGPU := ""
	for _, p := range pods {
		total := p.VRAMUsedMiB + p.VRAMFreeMiB
		podCell := p.Pod
		if p.GPUIndex != "" {
			fallback = true
			if len(p.HintPods) > 0 {
				podCell = podCell + " → " + strings.Join(p.HintPods, ",")
			}
		}
		gpuCell := strings.Join(p.GPUIndices, ",")
		if gpuCell == "" {
			gpuCell = fmt.Sprintf("(%d)", p.GPUCount)
		}
		curGPU := gpuKey(p)
		if prevGPU != "" && prevGPU != curGPU {
			lines = append(lines, "")
		}
		prevGPU = curGPU

		var b strings.Builder
		fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %-*s  ",
			tableWidths[0], truncate(p.Namespace, tableWidths[0]),
			tableWidths[1], truncate(podCell, tableWidths[1]),
			tableWidths[2], truncate(p.Node, tableWidths[2]),
			tableWidths[3], gpuCell,
		)
		utilStr := fmt.Sprintf("%5.1f%%", p.GPUUtilPct)
		b.WriteString(utilStyle(p.GPUUtilPct).Render(fmt.Sprintf("%-*s", tableWidths[4], utilStr)))
		b.WriteString("  ")
		fmt.Fprintf(&b, "%-*s  ", tableWidths[5], fmt.Sprintf("%6.0f/%-6.0f MiB", p.VRAMUsedMiB, total))
		fmt.Fprintf(&b, "%-*s", tableWidths[6], fmt.Sprintf("%5.1fW", p.PowerWatts))
		lines = append(lines, b.String())
	}
	return lines, fallback
}

// totalLines counts what renderBodyLines would produce, without doing the
// expensive string-building. Used for scroll-offset clamping in Update().
func (m Model) totalLines() int {
	if len(m.pods) == 0 {
		return 0
	}
	pods := m.sortedPods()
	n := len(pods)
	prev := ""
	for _, p := range pods {
		cur := gpuKey(p)
		if prev != "" && prev != cur {
			n++
		}
		prev = cur
	}
	return n
}

// bodyHeight is the number of rows of body content the viewport can show.
// Anchored to m.height; the chrome above and below the body claims
// reservedRows plus an extra ~3 lines for the fallback hint and ~2 for any
// error line (we overestimate slightly so the visible window doesn't run
// past the terminal edge).
func (m Model) bodyHeight() int {
	if m.height <= 0 {
		return 20 // first frame, before WindowSizeMsg
	}
	reserved := reservedRows
	if m.lastErr != nil {
		reserved += 2
	}
	h := m.height - reserved
	if h < 3 {
		return 3
	}
	return h
}

func (m Model) maxScroll() int {
	n := m.totalLines() - m.bodyHeight()
	if n < 0 {
		return 0
	}
	return n
}

func (m Model) View() string {
	bodyLines, fallback := m.renderBodyLines()
	avail := m.bodyHeight()
	if fallback {
		avail -= 3 // crude: fallback hint is ~3 lines, deduct from body
		if avail < 3 {
			avail = 3
		}
	}

	start := m.scrollOffset
	if start > len(bodyLines) {
		start = len(bodyLines)
	}
	end := start + avail
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[start:end]

	var b strings.Builder

	// --- top bar: title + status (with scroll range when applicable) ---
	ts := "—"
	if !m.lastScrape.IsZero() {
		ts = m.lastScrape.Format("15:04:05")
	}
	rangeStr := ""
	if len(bodyLines) > 0 {
		rangeStr = fmt.Sprintf(" · rows %d–%d/%d", start+1, end, len(bodyLines))
	}
	scrollHint := "[↑↓ PgUp PgDn g G] scroll"
	if len(bodyLines) <= avail {
		scrollHint = "" // nothing to scroll
	}
	status := fmt.Sprintf("last scrape %s%s · [q]uit  [r]efresh", ts, rangeStr)
	if scrollHint != "" {
		status += "  " + scrollHint
	}
	if m.scraping {
		status += " · scraping…"
	}
	b.WriteString(styleTitle.Render("kubectl-gpugo"))
	b.WriteString("  ")
	b.WriteString(styleHint.Render(status))
	b.WriteString("\n\n")

	// --- column header ---
	var hdr strings.Builder
	for i, c := range tableCols {
		fmt.Fprintf(&hdr, "%-*s  ", tableWidths[i], c)
	}
	b.WriteString(styleColHdr.Render(hdr.String()))
	b.WriteString("\n")

	// --- body ---
	if len(bodyLines) == 0 {
		if m.lastErr == nil && !m.lastScrape.IsZero() {
			b.WriteString(styleDim.Render("  (no GPU metrics returned by any exporter)\n"))
		}
	} else {
		b.WriteString(strings.Join(visible, "\n"))
		b.WriteString("\n")
	}

	// --- footer: fallback hint, error ---
	if fallback {
		b.WriteString(styleDim.Render("dcgm-exporter is not emitting pod labels — rows grouped per (node, GPU);\n"))
		b.WriteString(styleDim.Render("→ lists pods on that node requesting nvidia.com/gpu. Enable --kubernetes\n"))
		b.WriteString(styleDim.Render("on dcgm-exporter for per-pod attribution.\n"))
	}
	if m.lastErr != nil {
		b.WriteString(styleErr.Render(fmt.Sprintf("scrape error: %v", m.lastErr)))
		b.WriteString("\n")
	}

	return b.String()
}

// gpuKey returns a sortable key built from a pod's GPU indices so the table
// groups rows by which card they're on. Components are zero-padded so
// "0:8" sorts before "0:10" lexicographically.
func gpuKey(p scraper.PodGPU) string {
	if len(p.GPUIndices) == 0 {
		return "~"
	}
	var b strings.Builder
	for _, g := range p.GPUIndices {
		for _, part := range strings.Split(g, ":") {
			if len(part) < 3 {
				b.WriteString(strings.Repeat("0", 3-len(part)))
			}
			b.WriteString(part)
			b.WriteByte(':')
		}
		b.WriteByte(',')
	}
	return b.String()
}

// truncate keeps the table aligned when a pod or namespace name overflows its
// column. Anything 4 chars or wider gets an ellipsis; below that we hard-cut
// to avoid a single ellipsis taking up the whole cell.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
