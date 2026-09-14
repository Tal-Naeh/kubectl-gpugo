package tui

import (
	"strings"
	"testing"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/scraper"
)

// Rows spanning several physical cards (MIG slices and whole GPUs) must
// render as one continuous block: one line per row, no blank separators.
func TestRenderBodyLinesIsContinuous(t *testing.T) {
	pods := []scraper.PodGPU{
		{Namespace: "a", Pod: "p1", Node: "n", GPUIndices: []string{"0:8"}, GPUCount: 1},
		{Namespace: "a", Pod: "p2", Node: "n", GPUIndices: []string{"0:10"}, GPUCount: 1},
		{Namespace: "b", Pod: "p3", Node: "n", GPUIndices: []string{"1:8"}, GPUCount: 1},
		{Namespace: "b", Pod: "p4", Node: "n", GPUIndices: []string{"2"}, GPUCount: 1},
		{Namespace: "c", Pod: "p5", Node: "n", GPUIndices: []string{"3"}, GPUCount: 1},
		{Namespace: "c", Pod: "p6", Node: "n", GPUIndices: nil, GPUCount: 2},
	}
	m := Model{pods: pods}
	lines, fallback := m.renderBodyLines()
	if fallback {
		t.Fatalf("no fallback rows expected")
	}
	if len(lines) != len(pods) {
		t.Fatalf("got %d lines for %d rows", len(lines), len(pods))
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			t.Fatalf("line %d is blank; body must have no separators", i)
		}
	}
	if got := m.totalLines(); got != len(pods) {
		t.Fatalf("totalLines = %d, want %d", got, len(pods))
	}
}
