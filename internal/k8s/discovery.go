package k8s

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ExporterPod is the subset of pod metadata the scraper actually needs.
type ExporterPod struct {
	Namespace string
	Name      string
	Port      int32
	NodeName  string
}

// dcgm-exporter ships under a few different label conventions depending on
// whether it was installed via the NVIDIA gpu-operator Helm chart, the
// standalone dcgm-exporter chart, or a hand-rolled manifest. Try them in turn
// and stop at the first one that returns pods.
var exporterSelectors = []string{
	"app.kubernetes.io/name=dcgm-exporter",
	"app=dcgm-exporter",
	"app.kubernetes.io/component=dcgm-exporter",
}

// FindExporters returns the running dcgm-exporter pods across all namespaces.
// Pods that aren't Ready are filtered out — proxying to them just produces
// connection-refused errors that pollute the TUI.
func FindExporters(ctx context.Context, cs kubernetes.Interface) ([]ExporterPod, error) {
	var found []corev1.Pod
	for _, sel := range exporterSelectors {
		list, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return nil, fmt.Errorf("list pods (%s): %w", sel, err)
		}
		if len(list.Items) > 0 {
			found = list.Items
			break
		}
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no dcgm-exporter pods found (tried selectors: %v)", exporterSelectors)
	}

	out := make([]ExporterPod, 0, len(found))
	for _, p := range found {
		if p.Status.Phase != corev1.PodRunning || !podReady(p) {
			continue
		}
		port := metricsPort(p)
		if port == 0 {
			continue
		}
		out = append(out, ExporterPod{
			Namespace: p.Namespace,
			Name:      p.Name,
			Port:      port,
			NodeName:  p.Spec.NodeName,
		})
	}
	return out, nil
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// metricsPort picks the container port that exposes Prometheus metrics. The
// canonical dcgm-exporter port is 9400 (named "metrics"), but we fall back to
// any HTTP-ish named port and finally to 9400 to handle minimal manifests
// that don't name their ports.
func metricsPort(p corev1.Pod) int32 {
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == "metrics" || port.Name == "http-metrics" {
				return port.ContainerPort
			}
		}
	}
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if port.ContainerPort == 9400 {
				return port.ContainerPort
			}
		}
	}
	return 9400
}
