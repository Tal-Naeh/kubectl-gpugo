package scraper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"
)

func loadFixture(t *testing.T, name string) map[string]*dto.MetricFamily {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	p := expfmt.NewTextParser(prommodel.LegacyValidation)
	fams, err := p.TextToMetricFamilies(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return fams
}

func rowByPod(t *testing.T, rows []PodGPU, ns, pod string) PodGPU {
	t.Helper()
	for _, r := range rows {
		if r.Namespace == ns && r.Pod == pod {
			return r
		}
	}
	t.Fatalf("row %s/%s not found in %+v", ns, pod, rows)
	return PodGPU{}
}

func TestDCGMPodAttribution(t *testing.T) {
	samples := extractSamples(loadFixture(t, "dcgm_pod_labels.prom"), "exporter-node")
	rows := aggregateByPod(samples)
	if len(rows) != 2 {
		t.Fatalf("want 2 pod rows (unattributed GPU 3 dropped), got %d: %+v", len(rows), rows)
	}

	vllm := rowByPod(t, rows, "ml", "vllm-0")
	if vllm.GPUCount != 2 || strings.Join(vllm.GPUIndices, ",") != "0,1" {
		t.Errorf("vllm-0 gpus: count=%d idx=%v", vllm.GPUCount, vllm.GPUIndices)
	}
	if vllm.GPUUtilPct != 65 { // (87+43)/2
		t.Errorf("vllm-0 util: want 65, got %v", vllm.GPUUtilPct)
	}
	if vllm.VRAMUsedMiB != 135000 || vllm.VRAMTotalMiB() != 162000 {
		t.Errorf("vllm-0 vram: used=%v total=%v", vllm.VRAMUsedMiB, vllm.VRAMTotalMiB())
	}
	if vllm.PowerWatts != 511 {
		t.Errorf("vllm-0 power: want 511, got %v", vllm.PowerWatts)
	}
	if vllm.Node != "node-a" { // Hostname label wins over exporter node
		t.Errorf("vllm-0 node: want node-a, got %q", vllm.Node)
	}

	tei := rowByPod(t, rows, "embeddings", "tei-7d9f-x1")
	if tei.GPUCount != 1 || tei.GPUUtilPct != 12 || tei.VRAMUsedMiB != 4096 {
		t.Errorf("tei row wrong: %+v", tei)
	}
}

func TestDCGMFallbackPerGPU(t *testing.T) {
	// Strip pod labels by aggregating the unattributed samples only.
	samples := extractSamples(loadFixture(t, "dcgm_pod_labels.prom"), "exporter-node")
	var unattributed []sample
	for _, s := range samples {
		if s.pod == "" {
			unattributed = append(unattributed, s)
		}
	}
	rows := aggregateByGPU(unattributed)
	if len(rows) != 1 {
		t.Fatalf("want 1 fallback row, got %d", len(rows))
	}
	r := rows[0]
	if !r.IsFallback() || r.GPUIndex != "3" || r.Pod != "(gpu 3)" || r.Namespace != "-" {
		t.Errorf("fallback row shape wrong: %+v", r)
	}
	if r.VRAMTotalMiB() != 81000 || r.PowerWatts != 55 {
		t.Errorf("fallback row values wrong: %+v", r)
	}
}

func TestDCGMMIGSlices(t *testing.T) {
	samples := extractSamples(loadFixture(t, "dcgm_mig.prom"), "dgx-1")
	rows := aggregateByPod(samples)
	if len(rows) != 4 {
		t.Fatalf("want 4 rows (one per slice-holding pod), got %d", len(rows))
	}
	tr0 := rowByPod(t, rows, "it-dgx1", "transcription-0")
	if strings.Join(tr0.GPUIndices, ",") != "0:7" {
		t.Errorf("MIG slice id not keyed as gpu:GPU_I_ID: %v", tr0.GPUIndices)
	}
	if tr0.GPUUtilPct != 42 { // PROF_GR_ENGINE_ACTIVE 0.42 -> 42%
		t.Errorf("MIG util scaling: want 42, got %v", tr0.GPUUtilPct)
	}
	ocr := rowByPod(t, rows, "it-dgx2", "ocr-0")
	if ocr.VRAMUsedMiB != 9000 || ocr.VRAMTotalMiB() != 9700 {
		t.Errorf("ocr vram wrong: %+v", ocr)
	}

	// All three slices of GPU 0 must sort together, before GPU 1.
	sorted := SortRows(rows)
	var order []string
	for _, r := range sorted {
		order = append(order, r.GPUIndices[0])
	}
	if got := strings.Join(order, " "); got != "0:7 0:8 0:9 1:1" { // slice id ascending, card 0 before card 1
		t.Errorf("sort order: %s", got)
	}
}

func TestEnricherRows(t *testing.T) {
	rows := buildEnricherRows([]enricherResult{{fams: loadFixture(t, "enricher.prom"), node: "gpu-node-1"}})
	if len(rows) != 3 {
		t.Fatalf("want 3 pod rows, got %d: %+v", len(rows), rows)
	}
	const gib = 1024
	vllm := rowByPod(t, rows, "ml", "vllm-0")
	if vllm.VRAMUsedMiB != 50*gib { // 40 GiB + 10 GiB across two pids
		t.Errorf("vllm vram: %v", vllm.VRAMUsedMiB)
	}
	if vllm.GPUUtilPct != 60 { // max across pids, not sum
		t.Errorf("vllm util: %v", vllm.GPUUtilPct)
	}
	// power share = 400W * (50/80)
	if vllm.PowerWatts < 249.9 || vllm.PowerWatts > 250.1 {
		t.Errorf("vllm power share: %v", vllm.PowerWatts)
	}
	tei1 := rowByPod(t, rows, "embeddings", "tei-1")
	if tei1.PowerWatts < 99.9 || tei1.PowerWatts > 100.1 { // 400 * 20/80
		t.Errorf("tei-1 power share: %v", tei1.PowerWatts)
	}
	if tei1.Node != "gpu-node-1" {
		t.Errorf("tei-1 node: %q", tei1.Node)
	}
	tei2 := rowByPod(t, rows, "embeddings", "tei-2")
	if strings.Join(tei2.GPUIndices, ",") != "1" {
		t.Errorf("tei-2 gpu idx: %v", tei2.GPUIndices)
	}
}

func TestFilterNamespace(t *testing.T) {
	rows := []PodGPU{
		{Namespace: "ml", Pod: "a"},
		{Namespace: "embeddings", Pod: "b"},
		{Namespace: "-", Pod: "(gpu 0)", GPUIndex: "0", HintPods: []string{"ml/x", "other/y"}},
		{Namespace: "-", Pod: "(gpu 1)", GPUIndex: "1", HintPods: []string{"other/z"}},
	}
	if got := FilterNamespace(rows, ""); len(got) != 4 {
		t.Errorf("empty ns must be a no-op, got %d rows", len(got))
	}
	got := FilterNamespace(rows, "ml")
	if len(got) != 2 {
		t.Fatalf("want pod row + 1 fallback row, got %+v", got)
	}
	if got[0].Pod != "a" {
		t.Errorf("first row: %+v", got[0])
	}
	if got[1].GPUIndex != "0" || strings.Join(got[1].HintPods, ",") != "ml/x" {
		t.Errorf("fallback hints not narrowed: %+v", got[1])
	}
}

func TestReportJSONAndTable(t *testing.T) {
	samples := extractSamples(loadFixture(t, "dcgm_pod_labels.prom"), "exporter-node")
	rep := NewReport(aggregateByPod(samples), time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	if rep.Mode != "pod" {
		t.Errorf("mode: %q", rep.Mode)
	}

	var buf bytes.Buffer
	if err := rep.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("json round-trip: %v\n%s", err, buf.String())
	}
	if len(back.Rows) != 2 || back.Rows[0].GPUIndices == nil {
		t.Errorf("json rows: %+v", back.Rows)
	}
	if !strings.Contains(buf.String(), `"gpus": [`) || strings.Contains(buf.String(), `"gpuIndex"`) {
		t.Errorf("json field naming: %s", buf.String())
	}

	buf.Reset()
	if err := rep.WriteTable(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "NAMESPACE") || !strings.Contains(out, "vllm-0") || !strings.Contains(out, "0,1") {
		t.Errorf("table output:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("table output must not contain ANSI escapes")
	}
}
