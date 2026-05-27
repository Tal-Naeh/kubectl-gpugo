// Package scraper turns a list of dcgm-exporter pods into a GPU snapshot.
// Every exporter is scraped through the kube-apiserver pod proxy subresource
// (GET /api/v1/namespaces/<ns>/pods/<name>:<port>/proxy/metrics) so we avoid
// local port-forwarding, firewall punch-throughs, and per-exporter goroutine
// lifecycles.
//
// Two aggregation modes are supported. When dcgm-exporter emits pod-level
// labels (the default with --kubernetes=true) we attribute per workload pod.
// When those labels are absent we fall back to per-(node, GPU) rows and
// cross-reference the cluster's GPU-requesting pods so each row carries a
// hint of which workloads *could* be using that GPU.
package scraper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/k8s"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// PodGPU is one row in the TUI table. In pod-attributed mode (preferred) it
// describes a workload pod and its GPUs. In fallback mode GPUIndex is set,
// Namespace is "-", Pod is "(gpu N)", and HintPods lists candidate workloads.
type PodGPU struct {
	Namespace   string
	Pod         string
	Node        string
	GPUIndex    string   // empty in pod-attributed mode
	HintPods    []string // candidates in fallback mode (ns/pod)
	GPUIndices  []string // sorted GPU indices used by this pod ("0","1","2" or "0,1")
	GPUCount    int
	GPUUtilPct  float64
	VRAMUsedMiB float64
	VRAMFreeMiB float64
	PowerWatts  float64
}

type Scraper struct {
	cs      kubernetes.Interface
	restCfg *rest.Config

	// discovery cache. Auto-detection probes every candidate pod's /metrics
	// to classify it; that's too expensive to do every 2s TUI tick, so we
	// memoise the result for discoveryTTL.
	discMu    sync.Mutex
	discCache []k8s.ExporterPod
	discTime  time.Time
}

const discoveryTTL = 30 * time.Second

// prometheus/common v0.67+ exposes a package-global "name validation scheme"
// (model.NameValidationScheme) that the expfmt TextParser consults; its zero
// value is "unset" and the parser panics with "Invalid name validation
// scheme requested: unset" if it hasn't been initialised. DCGM metric names
// (`DCGM_FI_DEV_GPU_UTIL`, etc.) are all legacy-format identifiers, so we
// pin the scheme to LegacyValidation here.
func init() {
	prommodel.NameValidationScheme = prommodel.LegacyValidation
}

func New(cs kubernetes.Interface, restCfg *rest.Config) *Scraper {
	return &Scraper{cs: cs, restCfg: restCfg}
}

// sample is the flattened form of a single DCGM metric reading. Everything
// downstream operates on []sample so the two aggregation passes can share the
// same input without re-scraping.
type sample struct {
	ns, pod, gpu, node, name string
	val                      float64
}

type gpuKey struct{ ns, pod, gpu string }

// Snapshot dispatches to the right backend based on what GPU exporters
// auto-discovery found. Enricher (cadvisor-gpu-gpu-enricher and friends)
// wins when available — its per-process labels give true pod attribution
// even for workloads that bypass the NVIDIA device plugin. DCGM is the
// fallback, with its own per-pod/per-GPU fallbacks layered inside.
func (s *Scraper) Snapshot(ctx context.Context) ([]PodGPU, error) {
	sources, err := s.discover(ctx, false)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no GPU metrics exporters discovered (no pod's /metrics emitted DCGM_FI_DEV_* or gpu_process_memory_bytes)")
	}

	var enrichers, dcgms []k8s.ExporterPod
	for _, src := range sources {
		switch src.Kind {
		case k8s.KindEnricher:
			enrichers = append(enrichers, src)
		case k8s.KindDCGM:
			dcgms = append(dcgms, src)
		}
	}

	if len(enrichers) > 0 {
		rows, err := s.snapshotFromEnrichers(ctx, enrichers)
		if err == nil && len(rows) > 0 {
			return rows, nil
		}
	}
	if len(dcgms) > 0 {
		return s.snapshotFromDCGM(ctx, dcgms)
	}
	return nil, nil
}

// snapshotFromDCGM implements the original DCGM-only path: per-pod
// attribution when DCGM emits pod labels, per-(node, GPU) fallback when it
// doesn't (with HintPods from a cluster-wide pod scan).
func (s *Scraper) snapshotFromDCGM(ctx context.Context, exporters []k8s.ExporterPod) ([]PodGPU, error) {
	samples, err := s.gather(ctx, exporters)
	if err != nil {
		return nil, err
	}

	if rows := aggregateByPod(samples); len(rows) > 0 {
		return rows, nil
	}

	rows := aggregateByGPU(samples)
	if len(rows) == 0 {
		return nil, nil
	}
	if hints, err := s.gpuRequestingPodsByNode(ctx); err == nil {
		for i := range rows {
			rows[i].HintPods = hints[rows[i].Node]
		}
	}
	return rows, nil
}

// discover returns the cached list of GPU exporters, refreshing when stale
// (or when force=true). Probing every candidate pod's /metrics is too costly
// to do every 2s; pods come and go on a much slower timescale.
func (s *Scraper) discover(ctx context.Context, force bool) ([]k8s.ExporterPod, error) {
	s.discMu.Lock()
	defer s.discMu.Unlock()
	if !force && time.Since(s.discTime) < discoveryTTL && len(s.discCache) > 0 {
		return s.discCache, nil
	}
	sources, err := k8s.DiscoverGPUExporters(ctx, s.cs)
	if err != nil {
		return nil, err
	}
	s.discCache = sources
	s.discTime = time.Now()
	return sources, nil
}

// DumpPod writes the raw /metrics body of an explicit pod to w. Format of
// target: "namespace/pod-name:port" where port is numeric. Used by the
// `--dump-pod` flag when investigating arbitrary exporters that the
// auto-discovery wouldn't have found.
func (s *Scraper) DumpPod(ctx context.Context, target string, w io.Writer) error {
	nsRest := strings.SplitN(target, "/", 2)
	if len(nsRest) != 2 {
		return fmt.Errorf("bad target %q, want namespace/pod:port", target)
	}
	ns := nsRest[0]
	nameRest := strings.SplitN(nsRest[1], ":", 2)
	if len(nameRest) != 2 {
		return fmt.Errorf("bad target %q, want namespace/pod:port", target)
	}
	name, port := nameRest[0], nameRest[1]
	data, err := s.cs.CoreV1().Pods(ns).
		ProxyGet("http", name, port, "metrics", nil).DoRaw(ctx)
	if err != nil {
		return fmt.Errorf("proxy GET %s/%s:%s: %w", ns, name, port, err)
	}
	_, err = w.Write(data)
	return err
}

// Dump writes the raw /metrics body of every auto-discovered GPU exporter
// (dcgm, enricher, or anything else the classifier recognises) to w. Useful
// for inspecting label conventions across exporters.
func (s *Scraper) Dump(ctx context.Context, w io.Writer) error {
	sources, err := s.discover(ctx, true) // force-refresh so --dump always reflects current state
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		fmt.Fprintln(w, "no GPU exporters discovered")
		return nil
	}
	for _, ex := range sources {
		fmt.Fprintf(w, "===== [%s] %s/%s on node %q :%d =====\n", ex.Kind, ex.Namespace, ex.Name, ex.NodeName, ex.Port)
		data, err := s.cs.CoreV1().Pods(ex.Namespace).
			ProxyGet("http", ex.Name, strconv.Itoa(int(ex.Port)), "metrics", nil).DoRaw(ctx)
		if err != nil {
			fmt.Fprintf(w, "ERROR: %v\n\n", err)
			continue
		}
		_, _ = w.Write(data)
		fmt.Fprintln(w)
	}
	return nil
}

func (s *Scraper) gather(ctx context.Context, exporters []k8s.ExporterPod) ([]sample, error) {
	var (
		mu       sync.Mutex
		out      []sample
		firstErr error
		wg       sync.WaitGroup
	)
	for _, ex := range exporters {
		wg.Add(1)
		go func(ex k8s.ExporterPod) {
			defer wg.Done()
			fams, err := s.scrapeOne(ctx, ex)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			ss := extractSamples(fams, ex.NodeName)
			mu.Lock()
			out = append(out, ss...)
			mu.Unlock()
		}(ex)
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// scrapeOne reads the metrics body fully into memory before parsing so we can
// include a snippet on parse error, and converts any prometheus-parser panic
// into a normal returned error.
func (s *Scraper) scrapeOne(ctx context.Context, ex k8s.ExporterPod) (fams map[string]*dto.MetricFamily, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic on %s/%s: %v", ex.Namespace, ex.Name, r)
			fams = nil
		}
	}()

	req := s.cs.CoreV1().Pods(ex.Namespace).
		ProxyGet("http", ex.Name, strconv.Itoa(int(ex.Port)), "metrics", nil)
	data, err := req.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy GET %s/%s: %w", ex.Namespace, ex.Name, err)
	}
	// In prometheus/common v0.67, expfmt.TextParser carries its OWN scheme
	// field (parser-local, NOT a reference to model.NameValidationScheme).
	// The zero value is UnsetValidation, which makes the parser panic with
	// "Invalid name validation scheme requested: unset" on the first label
	// it tries to validate. NewTextParser constructs one with the scheme set
	// explicitly. DCGM and the enricher both emit legacy-format identifiers,
	// so LegacyValidation is correct.
	p := expfmt.NewTextParser(prommodel.LegacyValidation)
	fams, err = p.TextToMetricFamilies(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s/%s: %w (first 160B: %q)", ex.Namespace, ex.Name, err, snippet(data, 160))
	}
	return fams, nil
}

var wantedMetrics = []string{
	"DCGM_FI_DEV_GPU_UTIL",
	"DCGM_FI_DEV_FB_USED",
	"DCGM_FI_DEV_FB_FREE",
	"DCGM_FI_DEV_POWER_USAGE",
	// DCGM_FI_PROF_GR_ENGINE_ACTIVE is the per-MIG-slice graphics-engine
	// activity ratio [0,1]; MIG-configured DCGM installs disable
	// DCGM_FI_DEV_GPU_UTIL entirely (utilisation isn't meaningful per slice)
	// so we treat PROF_GR_ENGINE_ACTIVE as the util replacement on MIG.
	"DCGM_FI_PROF_GR_ENGINE_ACTIVE",
}

func extractSamples(fams map[string]*dto.MetricFamily, exporterNode string) []sample {
	var out []sample
	for _, name := range wantedMetrics {
		f, ok := fams[name]
		if !ok {
			continue
		}
		for _, m := range f.Metric {
			ns, pod, gpu, gpuI, host := readLabels(m.Label)
			node := host
			if node == "" {
				node = exporterNode
			}
			// On MIG installs each metric line carries both `gpu` (the
			// physical card) and `GPU_I_ID` (the MIG slice on that card).
			// Multiple slices on the same card go to different pods, so we
			// must key the "GPU" identity on both — otherwise four pods
			// sharing physical GPU 0 collapse into a single row.
			if gpuI != "" {
				gpu = gpu + ":" + gpuI
			}
			out = append(out, sample{ns: ns, pod: pod, gpu: gpu, node: node, name: name, val: metricValue(m)})
		}
	}
	return out
}

func aggregateByPod(samples []sample) []PodGPU {
	agg := map[string]*PodGPU{}
	seenGPU := map[gpuKey]struct{}{}
	utilSum := map[string]float64{}
	for _, s := range samples {
		if s.ns == "" || s.pod == "" {
			continue
		}
		key := s.ns + "/" + s.pod
		p, ok := agg[key]
		if !ok {
			p = &PodGPU{Namespace: s.ns, Pod: s.pod, Node: s.node}
			agg[key] = p
		} else if p.Node == "" {
			p.Node = s.node
		}
		applySample(p, key, s.name, s.gpu, s.val, seenGPU, utilSum)
	}
	return finalize(agg, utilSum)
}

func aggregateByGPU(samples []sample) []PodGPU {
	agg := map[string]*PodGPU{}
	seenGPU := map[gpuKey]struct{}{}
	utilSum := map[string]float64{}
	for _, s := range samples {
		gpu := s.gpu
		if gpu == "" {
			gpu = "?"
		}
		key := s.node + "/" + gpu
		p, ok := agg[key]
		if !ok {
			p = &PodGPU{
				Namespace:  "-",
				Pod:        fmt.Sprintf("(gpu %s)", gpu),
				Node:       s.node,
				GPUIndex:   gpu,
				GPUIndices: []string{gpu},
			}
			agg[key] = p
		}
		applySample(p, key, s.name, gpu, s.val, seenGPU, utilSum)
	}
	return finalize(agg, utilSum)
}

func applySample(
	p *PodGPU, key, metric, gpu string, v float64,
	seenGPU map[gpuKey]struct{}, utilSum map[string]float64,
) {
	// Register the (pod, gpu) pair regardless of which metric brought us
	// here. MIG installs don't emit DCGM_FI_DEV_GPU_UTIL at all, so if we
	// counted GPUs only from that metric, every pod's GPUCount would stay
	// at 0 and the rows would never reach the TUI.
	k := gpuKey{p.Namespace, p.Pod, gpu}
	if _, ok := seenGPU[k]; !ok {
		seenGPU[k] = struct{}{}
		p.GPUCount++
		if gpu != "" {
			p.GPUIndices = append(p.GPUIndices, gpu)
		}
	}

	switch metric {
	case "DCGM_FI_DEV_GPU_UTIL":
		utilSum[key] += v
	case "DCGM_FI_PROF_GR_ENGINE_ACTIVE":
		// PROF metrics are reported as ratio [0,1]; scale to match the
		// 0–100 semantics of DCGM_FI_DEV_GPU_UTIL so the TUI's % column
		// is consistent regardless of which one the exporter emits.
		utilSum[key] += v * 100
	case "DCGM_FI_DEV_FB_USED":
		p.VRAMUsedMiB += v
	case "DCGM_FI_DEV_FB_FREE":
		p.VRAMFreeMiB += v
	case "DCGM_FI_DEV_POWER_USAGE":
		p.PowerWatts += v
	}
}

func finalize(agg map[string]*PodGPU, utilSum map[string]float64) []PodGPU {
	out := make([]PodGPU, 0, len(agg))
	for k, p := range agg {
		if p.GPUCount > 0 {
			p.GPUUtilPct = utilSum[k] / float64(p.GPUCount)
		}
		if len(p.GPUIndices) > 1 {
			sort.Strings(p.GPUIndices)
		}
		out = append(out, *p)
	}
	return out
}

// readLabels pulls the namespace/pod/gpu/node identifiers off a DCGM metric.
// The dcgm-exporter `--kubernetes` flag emits `namespace`, `pod`, `container`;
// when re-scraped through Prometheus relabel rules the original labels can be
// shifted to `exported_namespace`/`exported_pod`, so we accept both. The GPU
// index is named `gpu`; the MIG slice is `GPU_I_ID` (only present on
// MIG-configured cards). `UUID` is a fallback when no `gpu` is present.
func readLabels(labels []*dto.LabelPair) (ns, pod, gpu, gpuI, host string) {
	var nsAlt, podAlt string
	for _, l := range labels {
		switch l.GetName() {
		case "namespace":
			ns = l.GetValue()
		case "exported_namespace":
			nsAlt = l.GetValue()
		case "pod":
			pod = l.GetValue()
		case "exported_pod":
			podAlt = l.GetValue()
		case "gpu", "device":
			if gpu == "" {
				gpu = l.GetValue()
			}
		case "UUID":
			if gpu == "" {
				gpu = l.GetValue()
			}
		case "GPU_I_ID":
			gpuI = l.GetValue()
		case "Hostname":
			host = l.GetValue()
		}
	}
	if ns == "" {
		ns = nsAlt
	}
	if pod == "" {
		pod = podAlt
	}
	return
}

func metricValue(m *dto.Metric) float64 {
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Untyped != nil:
		return m.Untyped.GetValue()
	}
	return 0
}

func snippet(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return string(b)
}

// gpuRequestingPodsByNode lists Running pods that request nvidia.com/gpu,
// grouped by their scheduling node. Used to enrich the fallback view with
// "pods that *could* be using that GPU" hints.
func (s *Scraper) gpuRequestingPodsByNode(ctx context.Context) (map[string][]string, error) {
	list, err := s.cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, p := range list.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if !podRequestsGPU(p) {
			continue
		}
		out[p.Spec.NodeName] = append(out[p.Spec.NodeName], p.Namespace+"/"+p.Name)
	}
	return out, nil
}

func podRequestsGPU(p corev1.Pod) bool {
	hasGPU := func(rl corev1.ResourceList) bool {
		for k, q := range rl {
			if k == "nvidia.com/gpu" && q.Value() > 0 {
				return true
			}
		}
		return false
	}
	for _, c := range p.Spec.Containers {
		if hasGPU(c.Resources.Requests) || hasGPU(c.Resources.Limits) {
			return true
		}
	}
	return false
}
