package scraper

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Report is the non-interactive (--once / --output json) view of one
// snapshot. It is the stable, scriptable contract: fields are additive only.
type Report struct {
	ScrapedAt time.Time `json:"scrapedAt"`
	// Mode is "pod" when rows are attributed to workload pods, "gpu" when the
	// exporter gave no pod labels and rows are per-(node, GPU).
	Mode string   `json:"mode"`
	Rows []PodGPU `json:"rows"`
}

// NewReport wraps rows in a Report, sorted deterministically (by GPU, then
// VRAM desc, then name) so consecutive runs diff cleanly.
func NewReport(rows []PodGPU, at time.Time) Report {
	sorted := SortRows(rows)
	mode := "pod"
	for _, r := range sorted {
		if r.IsFallback() {
			mode = "gpu"
			break
		}
	}
	if sorted == nil {
		sorted = []PodGPU{}
	}
	return Report{ScrapedAt: at.UTC(), Mode: mode, Rows: sorted}
}

// SortRows orders rows for display: by first GPU index (so rows on the same
// physical card / MIG slice group together), then by VRAM-used desc within a
// GPU, then alphabetically. Shared by the TUI and the plain-text output.
func SortRows(rows []PodGPU) []PodGPU {
	out := append([]PodGPU(nil), rows...)
	sort.Slice(out, func(i, j int) bool {
		gi, gj := GPUSortKey(out[i]), GPUSortKey(out[j])
		if gi != gj {
			return gi < gj
		}
		if out[i].VRAMUsedMiB != out[j].VRAMUsedMiB {
			return out[i].VRAMUsedMiB > out[j].VRAMUsedMiB
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Pod < out[j].Pod
	})
	return out
}

// GPUSortKey returns a sortable key built from a pod's GPU indices so tables
// group rows by which card they're on. Components are zero-padded so "0:8"
// sorts before "0:10" lexicographically.
func GPUSortKey(p PodGPU) string {
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

// WriteJSON emits the report as indented JSON followed by a newline.
func (r Report) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteTable emits a kubectl-style plain table (no colour, no TUI chrome),
// suitable for piping into grep/awk or a k9s plugin pane.
func (r Report) WriteTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "NAMESPACE\tPOD\tNODE\tGPU\tGPU%\tVRAM USED\tVRAM TOTAL\tPOWER")
	for _, p := range r.Rows {
		podCell := p.Pod
		if p.IsFallback() && len(p.HintPods) > 0 {
			podCell += " -> " + strings.Join(p.HintPods, ",")
		}
		gpuCell := strings.Join(p.GPUIndices, ",")
		if gpuCell == "" {
			gpuCell = fmt.Sprintf("(%d)", p.GPUCount)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%.1f\t%.0fMi\t%.0fMi\t%.1fW\n",
			p.Namespace, podCell, p.Node, gpuCell, p.GPUUtilPct, p.VRAMUsedMiB, p.VRAMTotalMiB(), p.PowerWatts)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if r.Mode == "gpu" {
		fmt.Fprintln(w, "\n# dcgm-exporter is not emitting pod labels; rows are per (node, GPU) and '->' lists pods on that node requesting nvidia.com/gpu.")
	}
	return nil
}
