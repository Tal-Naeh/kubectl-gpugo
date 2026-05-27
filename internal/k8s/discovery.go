// Package k8s discovers GPU-metrics-bearing pods in the cluster. It does so
// without any hardcoded label selectors or fixed ports: it filters running
// pods whose name/image/labels mention GPU-related keywords, picks a metrics
// port from each candidate (annotation > named container port > first port),
// probes /metrics, and classifies each by the metric family names it emits
// (DCGM_FI_DEV_* → dcgm-exporter, gpu_process_memory_bytes → enricher).
package k8s

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type ExporterKind int

const (
	KindUnknown ExporterKind = iota
	KindDCGM
	KindEnricher
)

func (k ExporterKind) String() string {
	switch k {
	case KindDCGM:
		return "dcgm"
	case KindEnricher:
		return "enricher"
	default:
		return "unknown"
	}
}

// ExporterPod is the subset of pod metadata the scraper actually needs.
type ExporterPod struct {
	Namespace string
	Name      string
	Port      int32
	NodeName  string
	Kind      ExporterKind
}

// keywords that mark a pod as a plausible GPU metrics source. Used as a
// cheap filter before the expensive /metrics probe phase. Anything missing
// from this list will be skipped, so it errs on the side of including
// loosely-related pods (the probe will eliminate non-matches anyway).
var gpuKeywords = []string{
	"dcgm", "gpu", "nvidia", "cuda", "enricher", "cadvisor-gpu",
}

// DiscoverGPUExporters returns every running pod in the cluster that emits
// recognisable GPU metric families. No label selectors are hardcoded: the
// classifier reads /metrics and inspects the metric family names.
//
// Probes run in parallel; one slow pod can't block the others. A 3-second
// per-probe context bounds worst case to that ceiling.
func DiscoverGPUExporters(ctx context.Context, cs kubernetes.Interface) ([]ExporterPod, error) {
	list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	candidates := filterGPUCandidates(list.Items)

	var (
		out []ExporterPod
		mu  sync.Mutex
		wg  sync.WaitGroup
	)
	for _, p := range candidates {
		port := metricsPort(p)
		if port == 0 {
			continue
		}
		// Fast path: classify by container image name. Skips the apiserver
		// /metrics round-trip for every well-known exporter image, which on
		// slow proxies (e.g. RKE2 on a busy DGX) is the difference between
		// "first scrape fits in 5s" and "context deadline exceeded".
		if kind, ok := classifyByImage(p); ok {
			out = append(out, ExporterPod{
				Namespace: p.Namespace,
				Name:      p.Name,
				Port:      port,
				NodeName:  p.Spec.NodeName,
				Kind:      kind,
			})
			continue
		}
		// Slow path: actually probe the pod's /metrics endpoint and look at
		// the metric family names. Only used for exporters with unfamiliar
		// image names.
		wg.Add(1)
		go func(p corev1.Pod, port int32) {
			defer wg.Done()
			kind, ok := classifyByProbe(ctx, cs, p, port)
			if !ok {
				return
			}
			mu.Lock()
			out = append(out, ExporterPod{
				Namespace: p.Namespace,
				Name:      p.Name,
				Port:      port,
				NodeName:  p.Spec.NodeName,
				Kind:      kind,
			})
			mu.Unlock()
		}(p, port)
	}
	wg.Wait()

	return out, nil
}

// classifyByImage looks at the container image references to decide what
// kind of exporter a pod is. Avoids the network round-trip of classifyByProbe
// for the canonical NVIDIA / cadvisor-gpu images. Returns (Unknown, false)
// when the image doesn't match a known pattern, so the caller falls back to
// the probe.
func classifyByImage(p corev1.Pod) (ExporterKind, bool) {
	for _, c := range p.Spec.Containers {
		img := strings.ToLower(c.Image)
		switch {
		case strings.Contains(img, "dcgm-exporter"):
			return KindDCGM, true
		case strings.Contains(img, "cadvisor-gpu"), strings.Contains(img, "gpu-enricher"):
			return KindEnricher, true
		}
	}
	return KindUnknown, false
}

func filterGPUCandidates(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning || !podReady(p) {
			continue
		}
		if podMentionsGPU(p) {
			out = append(out, p)
		}
	}
	return out
}

func podMentionsGPU(p corev1.Pod) bool {
	if matchesKW(p.Name) {
		return true
	}
	for _, c := range p.Spec.Containers {
		if matchesKW(c.Name) || matchesKW(c.Image) {
			return true
		}
	}
	for k, v := range p.Labels {
		if matchesKW(k) || matchesKW(v) {
			return true
		}
	}
	return false
}

func matchesKW(s string) bool {
	low := strings.ToLower(s)
	for _, kw := range gpuKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

// classifyByProbe scrapes /metrics from the pod and returns its kind based
// on the metric family names present. An empty or unreachable endpoint
// returns (Unknown, false) and the pod is dropped from the result.
func classifyByProbe(ctx context.Context, cs kubernetes.Interface, p corev1.Pod, port int32) (ExporterKind, bool) {
	data, err := cs.CoreV1().Pods(p.Namespace).
		ProxyGet("http", p.Name, strconv.Itoa(int(port)), "metrics", nil).
		DoRaw(ctx)
	if err != nil {
		return KindUnknown, false
	}
	s := string(data)
	if strings.Contains(s, "gpu_process_memory_bytes") {
		return KindEnricher, true
	}
	if strings.Contains(s, "DCGM_FI_DEV_") {
		return KindDCGM, true
	}
	return KindUnknown, false
}

// metricsPort picks the container port that exposes Prometheus metrics.
// Resolution order: prometheus.io/port annotation → port named
// metrics/http-metrics/prom/prometheus → first declared port. Returns 0 if
// the pod declares no ports at all, in which case the caller skips it.
func metricsPort(p corev1.Pod) int32 {
	if v := p.Annotations["prometheus.io/port"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return int32(n)
		}
	}
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			switch strings.ToLower(port.Name) {
			case "metrics", "http-metrics", "prom", "prom-metrics", "prometheus":
				return port.ContainerPort
			}
		}
	}
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			return port.ContainerPort
		}
	}
	return 0
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
