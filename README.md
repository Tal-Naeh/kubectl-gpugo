# kubectl-gpugo

A `top`-like TUI for per-pod GPU usage on Kubernetes — zero cluster footprint.

`kubectl-gpugo` auto-discovers `dcgm-exporter` (and optionally `cadvisor-gpu-gpu-enricher`) pods you already have running and scrapes them through the kube-apiserver pod-proxy subresource. **No DaemonSets installed, no port-forwards on your machine, no firewall changes.** Works with MIG.

## What it shows

| Column     | Meaning                                                                     |
|------------|-----------------------------------------------------------------------------|
| NAMESPACE  | Workload pod's namespace                                                    |
| POD        | Workload pod consuming the GPU                                              |
| NODE       | Node hosting the exporter that reported the metric                          |
| GPU        | GPU index(es) used — `2` (single), `0,1` (two cards), `0:8` (MIG slice 8 of GPU 0) |
| GPU%       | Activity across the pod's GPUs (colored by intensity)                       |
| VRAM USED  | Used / Total framebuffer, summed across the pod's GPUs / slices             |
| POWER      | Power draw in Watts (proportional share when GPUs are shared across pods)   |

<img width="1064" height="698" alt="image" src="https://github.com/user-attachments/assets/8244c764-1d00-4f2b-bb28-d5b02c4b0950" />


Rows are sorted by physical GPU, then by MIG slice ID. A blank line separates each physical card so pile-ups are obvious at a glance.

## Requirements

### On the cluster

- An NVIDIA-flavoured GPU exporter that auto-discovery recognises. Out of the box:
  - **`dcgm-exporter`** — image name contains `dcgm-exporter` (from the NVIDIA GPU Operator or the standalone chart)
  - **`cadvisor-gpu-gpu-enricher`** — image name contains `cadvisor-gpu` or `gpu-enricher` (Kaleidoo's per-process attribution sidecar)
  - Anything else that exposes `DCGM_FI_DEV_*` or `gpu_process_memory_bytes` family names on its `/metrics` (auto-classified by content probe).
- For **per-pod attribution** from dcgm-exporter alone: dcgm-exporter must run with `--kubernetes` enabled, so metrics carry `namespace`/`pod` labels. The NVIDIA GPU Operator does this by default. Without it, you fall back to per-(node, GPU) rows.
- For workloads using `NVIDIA_VISIBLE_DEVICES=all` to bypass the device plugin, dcgm-exporter can't attribute (kubelet's pod-resources API knows nothing). For that case you need an enricher like `cadvisor-gpu-gpu-enricher` deployed alongside.
- **MIG**: dcgm-exporter on MIG-configured cards emits `DCGM_FI_PROF_GR_ENGINE_ACTIVE` per slice instead of `DCGM_FI_DEV_GPU_UTIL`. We handle both.

### Your kubeconfig user / service account needs

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: kubectl-gpugo-reader
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["list", "get"]
- apiGroups: [""]
  resources: ["pods/proxy"]
  verbs: ["get"]
```

The `list pods` on all namespaces is for auto-discovery. The `pods/proxy` is what actually fetches `/metrics`. If your account can't be granted cluster-wide `list pods`, use the `--exporter` flag (below) to scope the tool to a single pod you know about — that only needs `pods/proxy` in the exporter's namespace.

### Won't work with

- **Lens / freelens kubeconfigs** — their local proxy doesn't pass pod-proxy subresource requests through. Use a direct kubeconfig.
- **Restricted RBAC tokens** that can't `get pods/proxy`. There's no way around this: the entire scrape path goes through that subresource.

## Install as a kubectl plugin

```sh
go install github.com/Tal-Naeh/kubectl-gpugo@latest
# put the resulting binary on your PATH, renamed to `kubectl-gpugo`
mv "$(go env GOPATH)/bin/kubectl-gpugo" /usr/local/bin/kubectl-gpugo
```

Then run:

```sh
kubectl gpugo
```

`kubectl` finds the plugin by the `kubectl-` prefix on PATH. Standard kubectl flags work (`--context`, `--kubeconfig`, `-n`, etc.) — they're routed via `genericclioptions`.

## Flags

| Flag                  | Purpose                                                                                                  |
|-----------------------|----------------------------------------------------------------------------------------------------------|
| `--kubeconfig`, `--context` | Standard kubectl flags                                                                              |
| `--exporter ns/pod:port`    | Skip auto-discovery and scrape a specific pod. Comma-separate or repeat the flag for multiple pods. |
| `--dump`              | Print raw `/metrics` from every auto-discovered exporter and exit. Useful for debugging label conventions. |
| `--dump-pod ns/pod:port` | Print raw `/metrics` from one specific pod and exit.                                                  |

## Keys inside the TUI

| Key              | Action            |
|------------------|-------------------|
| `q` / `Ctrl-C` / `Esc` | Quit         |
| `r`              | Force-refresh now |
| `↑` / `k`        | Scroll up one row |
| `↓` / `j`        | Scroll down       |
| `PgUp` / `b`     | Page up           |
| `PgDn` / `Space` / `f` | Page down   |
| `Home` / `g`     | Jump to top       |
| `End` / `G`      | Jump to bottom    |

## Local dev

```sh
go build ./...
./kubectl-gpugo
```

The tick interval is 20s; force a refresh with `r`.

## Limitations / known issues

- The tool scrapes through the apiserver pod-proxy subresource. Slow control planes (e.g. RKE2 on a busy DGX with many MIG slices) can take 5–10s per scrape. If the apiserver gets wedged, restart the dcgm-exporter pod.
- Image-name classification is a short hardcoded list. Forked images with unusual names still get caught by the `/metrics` probe fallback, but if both the name AND the metric families look unusual the pod is skipped. Use `--exporter` as an override.
- Power on shared GPUs (multiple pods on one card via MIG or `NVIDIA_VISIBLE_DEVICES=all`) is attributed proportionally to each pod's VRAM share — the most honest split DCGM data supports.
