# kubectl-gpugo

A `top`-like TUI for per-pod GPU usage on Kubernetes — zero cluster footprint.

`kubectl-gpugo` discovers `dcgm-exporter` pods you already have running (e.g. from the NVIDIA GPU Operator) and scrapes them through the kube-apiserver pod-proxy subresource. **No DaemonSets installed, no port-forwards on your machine, no firewall changes.**

## What it shows

| Column     | Meaning                                                      |
|------------|--------------------------------------------------------------|
| NAMESPACE  | Workload pod's namespace                                     |
| POD        | Workload pod consuming the GPU                               |
| NODE       | Node hosting the dcgm-exporter that reported the metric      |
| GPU        | Number of distinct GPUs attributed to the pod                |
| GPU%       | Mean utilisation across the pod's GPUs (colored by intensity)|
| VRAM USED  | Used / Total framebuffer, summed across the pod's GPUs       |
| POWER      | Summed power draw in Watts                                   |

## Requirements

- A cluster with `dcgm-exporter` running and exposing the standard DCGM metrics:
  `DCGM_FI_DEV_GPU_UTIL`, `DCGM_FI_DEV_FB_USED`, `DCGM_FI_DEV_FB_FREE`, `DCGM_FI_DEV_POWER_USAGE`.
- dcgm-exporter must be running with pod association enabled (`--kubernetes`), so metrics carry `namespace`/`pod` labels. The NVIDIA GPU Operator does this by default.
- Your kubeconfig user needs `get` on `pods/proxy` in the namespace(s) where dcgm-exporter runs.

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

## Local dev

```sh
go build ./...
./kubectl-gpugo
```

Keys inside the TUI:

- `q` / `Ctrl-C` / `Esc` — quit
- `r` — force-refresh now (otherwise it ticks every 2s)
