// Per-pod GPU attribution via cadvisor-gpu-gpu-enricher.
//
// The enricher solves the exact problem DCGM can't: when workloads grab GPUs
// via NVIDIA_VISIBLE_DEVICES=all (bypassing the device plugin), kubelet's
// pod-resources API has no allocation to report and DCGM falls back to
// unattributed per-GPU rows. The enricher reads /proc on the host (it's
// hostPID:true) and correlates each GPU process's PID back to its
// container/pod via cgroup paths, then exposes:
//
//   gpu_process_memory_bytes{namespace,pod,container,pid,gpu,uuid,...}
//   gpu_process_utilization_percent{...same labels...}
//   gpu_total_memory_bytes{gpu,uuid,...}
//   gpu_total_utilization_percent{...}
//   gpu_power_usage_watts{...}
//
// We aggregate the *_process_* metrics into per-pod rows and use *_total_*
// + *_power_* to compute a proportional power share for each pod on each
// GPU it occupies. That proportional split is the most honest answer when
// multiple pods share one GPU and DCGM only reports total per-GPU power.

package scraper

import (
	"context"
	"strings"
	"sync"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/k8s"
	dto "github.com/prometheus/client_model/go"
)

func (s *Scraper) snapshotFromEnrichers(ctx context.Context, enrichers []k8s.ExporterPod) ([]PodGPU, error) {
	type result struct {
		fams map[string]*dto.MetricFamily
		node string
	}
	results := make(chan result, len(enrichers))
	var (
		firstErr error
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	for _, ex := range enrichers {
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
			results <- result{fams: fams, node: ex.NodeName}
		}(ex)
	}
	wg.Wait()
	close(results)

	type gpuInfo struct {
		node      string
		totalVRAM float64 // bytes
		powerW    float64
	}
	gpus := map[string]*gpuInfo{} // key: uuid

	type podGPUUse struct {
		node      string
		vramBytes float64
		utilPct   float64 // max across pids
	}
	pods := map[string]map[string]*podGPUUse{} // ns/pod -> uuid -> use

	for r := range results {
		if r.fams == nil {
			continue
		}

		for _, m := range r.fams["gpu_process_memory_bytes"].GetMetric() {
			ns, podn, uuid := label(m, "namespace"), label(m, "pod"), label(m, "uuid")
			if ns == "" || podn == "" || uuid == "" {
				continue
			}
			key := ns + "/" + podn
			if pods[key] == nil {
				pods[key] = map[string]*podGPUUse{}
			}
			if pods[key][uuid] == nil {
				pods[key][uuid] = &podGPUUse{node: r.node}
			}
			pods[key][uuid].vramBytes += metricValue(m)
		}

		for _, m := range r.fams["gpu_process_utilization_percent"].GetMetric() {
			ns, podn, uuid := label(m, "namespace"), label(m, "pod"), label(m, "uuid")
			if ns == "" || podn == "" || uuid == "" {
				continue
			}
			key := ns + "/" + podn
			if pods[key] == nil {
				pods[key] = map[string]*podGPUUse{}
			}
			if pods[key][uuid] == nil {
				pods[key][uuid] = &podGPUUse{node: r.node}
			}
			if v := metricValue(m); v > pods[key][uuid].utilPct {
				pods[key][uuid].utilPct = v
			}
		}

		for _, m := range r.fams["gpu_total_memory_bytes"].GetMetric() {
			uuid := label(m, "uuid")
			if uuid == "" {
				continue
			}
			if gpus[uuid] == nil {
				gpus[uuid] = &gpuInfo{node: r.node}
			}
			gpus[uuid].totalVRAM = metricValue(m)
		}
		for _, m := range r.fams["gpu_power_usage_watts"].GetMetric() {
			uuid := label(m, "uuid")
			if uuid == "" {
				continue
			}
			if gpus[uuid] == nil {
				gpus[uuid] = &gpuInfo{node: r.node}
			}
			gpus[uuid].powerW = metricValue(m)
		}
	}

	if len(pods) == 0 && firstErr != nil {
		return nil, firstErr
	}

	const mib = 1024 * 1024
	rows := make([]PodGPU, 0, len(pods))
	for nsPod, uses := range pods {
		i := strings.IndexByte(nsPod, '/')
		if i < 0 {
			continue
		}
		ns, podn := nsPod[:i], nsPod[i+1:]

		var (
			totalUsed  float64
			totalGPU   float64
			powerShare float64
			maxUtil    float64
			node       string
		)
		for uuid, use := range uses {
			totalUsed += use.vramBytes
			if use.utilPct > maxUtil {
				maxUtil = use.utilPct
			}
			if node == "" {
				node = use.node
			}
			if g, ok := gpus[uuid]; ok {
				totalGPU += g.totalVRAM
				if g.totalVRAM > 0 {
					powerShare += g.powerW * (use.vramBytes / g.totalVRAM)
				}
			}
		}
		rows = append(rows, PodGPU{
			Namespace:   ns,
			Pod:         podn,
			Node:        node,
			GPUCount:    len(uses),
			GPUUtilPct:  maxUtil,
			VRAMUsedMiB: totalUsed / mib,
			VRAMFreeMiB: (totalGPU - totalUsed) / mib,
			PowerWatts:  powerShare,
		})
	}
	return rows, nil
}

func label(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
