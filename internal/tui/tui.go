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
	scrapeTimeout = 5 * time.Second
)

type tickMsg time.Time

type scrapeMsg struct {
	pods []scraper.PodGPU
	err  error
	at   time.Time
}

type Model struct {
	scr        *scraper.Scraper
	pods       []scraper.PodGPU
	lastErr    error
	lastScrape time.Time
	width      int
	height     int
}

func NewModel(s *scraper.Scraper) Model {
	return Model{scr: s}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(scrapeCmd(m.scr), tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// scrapeCmd runs a snapshot in the background. The timeout is enforced inside
// the command, not on the Bubbletea side, so a hung apiserver can never wedge
// the TUI past `scrapeTimeout` worth of work.
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
			return m, scrapeCmd(m.scr)
		}
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
	case tickMsg:
		return m, tea.Batch(scrapeCmd(m.scr), tickCmd())
	case scrapeMsg:
		m.lastErr = msg.err
		m.lastScrape = msg.at
		if msg.err == nil {
			m.pods = msg.pods
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

func (m Model) View() string {
	var b strings.Builder

	ts := "—"
	if !m.lastScrape.IsZero() {
		ts = m.lastScrape.Format("15:04:05")
	}
	b.WriteString(styleTitle.Render("kubectl-gpugo"))
	b.WriteString("  ")
	b.WriteString(styleHint.Render(fmt.Sprintf("last scrape %s · %d pods on GPUs · [q]uit  [r]efresh", ts, len(m.pods))))
	b.WriteString("\n\n")

	pods := append([]scraper.PodGPU(nil), m.pods...)
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].GPUUtilPct != pods[j].GPUUtilPct {
			return pods[i].GPUUtilPct > pods[j].GPUUtilPct
		}
		if pods[i].VRAMUsedMiB != pods[j].VRAMUsedMiB {
			return pods[i].VRAMUsedMiB > pods[j].VRAMUsedMiB
		}
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Pod < pods[j].Pod
	})

	cols := []string{"NAMESPACE", "POD", "NODE", "GPU", "GPU%", "VRAM USED", "POWER"}
	widths := []int{16, 36, 14, 4, 7, 22, 8}

	var hdr strings.Builder
	for i, c := range cols {
		fmt.Fprintf(&hdr, "%-*s  ", widths[i], c)
	}
	b.WriteString(styleColHdr.Render(hdr.String()))
	b.WriteString("\n")

	for _, p := range pods {
		total := p.VRAMUsedMiB + p.VRAMFreeMiB
		fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %-*d  ",
			widths[0], truncate(p.Namespace, widths[0]),
			widths[1], truncate(p.Pod, widths[1]),
			widths[2], truncate(p.Node, widths[2]),
			widths[3], p.GPUCount,
		)
		utilStr := fmt.Sprintf("%5.1f%%", p.GPUUtilPct)
		b.WriteString(utilStyle(p.GPUUtilPct).Render(fmt.Sprintf("%-*s", widths[4], utilStr)))
		b.WriteString("  ")
		fmt.Fprintf(&b, "%-*s  ", widths[5], fmt.Sprintf("%6.0f/%-6.0f MiB", p.VRAMUsedMiB, total))
		fmt.Fprintf(&b, "%-*s\n", widths[6], fmt.Sprintf("%5.1fW", p.PowerWatts))
	}

	if len(pods) == 0 && m.lastErr == nil && !m.lastScrape.IsZero() {
		b.WriteString(styleDim.Render("  (no pods currently using GPUs)\n"))
	}

	if m.lastErr != nil {
		b.WriteString("\n")
		b.WriteString(styleErr.Render(fmt.Sprintf("scrape error: %v", m.lastErr)))
		b.WriteString("\n")
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
