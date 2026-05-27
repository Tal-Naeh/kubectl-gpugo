// Package scraper turns a list of dcgm-exporter pods into a per-workload-pod
// GPU snapshot. It avoids local port-forwarding entirely: every exporter is
// scraped through the kube-apiserver pod proxy subresource
// (GET /api/v1/namespaces/<ns>/pods/<name>:<port>/proxy/metrics), which gives
// us a single auth path, no local TCP sockets, and no goroutine lifecycle to
// manage per exporter.
package scraper

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/k8s"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// PodGPU is the per-workload-pod aggregate the TUI renders. "Workload" pods
// are the consumers of GPUs — the dcgm-exporter pods themselves are not
// represented here; they are only the source of metrics.
type PodGPU struct {
	Namespace    string
	Pod          string
	Node         string
	GPUCount     int     // distinct GPUs attributed to this pod
	GPUUtilPct   float64 // mean across the pod's GPUs
	VRAMUsedMiB  float64 // summed across the pod's GPUs
	VRAMFreeMiB  float64 // summed; total = used + free at scrape time
	PowerWatts   float64 // summed
}

// Scraper holds the K8s client; one Scraper is reused for every tick.
type Scraper struct {
	cs      kubernetes.Interface
	restCfg *rest.Config
}

func New(cs kubernetes.Interface, restCfg *rest.Config) *Scraper {
	return &Scraper{cs: cs, restCfg: restCfg}
}

// gpuKey identifies a single physical GPU so we don't double-count when more
// than one DCGM_FI_DEV_* sample arrives for it.
type gpuKey struct{ ns, pod, gpu string }

// Snapshot scrapes every dcgm-exporter once (in parallel) and aggregates per
// workload pod. A single broken exporter is logged-by-omission rather than
// failing the snapshot — partial data is more useful than no data.
func (s *Scraper) Snapshot(ctx context.Context) ([]PodGPU, error) {
	exporters, err := k8s.FindExporters(ctx, s.cs)
	if err != nil {
		return nil, err
	}

	type result struct {
		families map[string]*dto.MetricFamily
		node     string
	}
	results := make(chan result, len(exporters))

	var wg sync.WaitGroup
	for _, ex := range exporters {
		wg.Add(1)
		go func(ex k8s.ExporterPod) {
			defer wg.Done()
			fams, err := s.scrapeOne(ctx, ex)
			if err != nil {
				return
			}
			results <- result{families: fams, node: ex.NodeName}
		}(ex)
	}
	wg.Wait()
	close(results)

	agg := map[string]*PodGPU{}
	seenGPU := map[gpuKey]struct{}{}
	utilSum := map[string]float64{}

	for r := range results {
		addSamples(r.families, r.node, agg, seenGPU, utilSum)
	}

	out := make([]PodGPU, 0, len(agg))
	for k, p := range agg {
		if p.GPUCount > 0 {
			p.GPUUtilPct = utilSum[k] / float64(p.GPUCount)
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *Scraper) scrapeOne(ctx context.Context, ex k8s.ExporterPod) (map[string]*dto.MetricFamily, error) {
	req := s.cs.CoreV1().Pods(ex.Namespace).
		ProxyGet("http", ex.Name, strconv.Itoa(int(ex.Port)), "metrics", nil)
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy GET %s/%s: %w", ex.Namespace, ex.Name, err)
	}
	defer rc.Close()
	var p expfmt.TextParser
	return p.TextToMetricFamilies(rc)
}

func addSamples(
	fams map[string]*dto.MetricFamily,
	node string,
	agg map[string]*PodGPU,
	seenGPU map[gpuKey]struct{},
	utilSum map[string]float64,
) {
	handle := func(name string, fn func(p *PodGPU, gpu, key string, v float64)) {
		f, ok := fams[name]
		if !ok {
			return
		}
		for _, m := range f.Metric {
			ns, pod, gpu := podLabels(m.Label)
			if ns == "" || pod == "" {
				continue
			}
			v := metricValue(m)
			key := ns + "/" + pod
			p, ok := agg[key]
			if !ok {
				p = &PodGPU{Namespace: ns, Pod: pod, Node: node}
				agg[key] = p
			} else if p.Node == "" {
				p.Node = node
			}
			fn(p, gpu, key, v)
		}
	}

	handle("DCGM_FI_DEV_GPU_UTIL", func(p *PodGPU, gpu, key string, v float64) {
		k := gpuKey{p.Namespace, p.Pod, gpu}
		if _, ok := seenGPU[k]; !ok {
			seenGPU[k] = struct{}{}
			p.GPUCount++
		}
		utilSum[key] += v
	})
	handle("DCGM_FI_DEV_FB_USED", func(p *PodGPU, _, _ string, v float64) {
		p.VRAMUsedMiB += v
	})
	handle("DCGM_FI_DEV_FB_FREE", func(p *PodGPU, _, _ string, v float64) {
		p.VRAMFreeMiB += v
	})
	handle("DCGM_FI_DEV_POWER_USAGE", func(p *PodGPU, _, _ string, v float64) {
		p.PowerWatts += v
	})
}

// podLabels pulls the namespace/pod/gpu identifiers off a DCGM metric. The
// dcgm-exporter `--kubernetes` flag emits `namespace`, `pod`, `container`;
// when re-scraped through Prometheus relabel rules the original labels can be
// shifted to `exported_namespace`/`exported_pod`, so we accept both. The GPU
// is named `gpu` in current dcgm-exporter releases and `UUID` in older ones.
func podLabels(labels []*dto.LabelPair) (ns, pod, gpu string) {
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
		case "gpu":
			gpu = l.GetValue()
		case "UUID":
			if gpu == "" {
				gpu = l.GetValue()
			}
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
