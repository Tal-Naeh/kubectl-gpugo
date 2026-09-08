# kubectl-gpugo

A `top`-like TUI for per-pod GPU usage on Kubernetes — zero cluster footprint.

`kubectl-gpugo` auto-discovers any GPU metrics exporter you already have running and scrapes it through the kube-apiserver pod-proxy subresource. **No DaemonSets installed, no port-forwards on your machine, no firewall changes.** Works with MIG.

Out of the box it understands two kinds of exporters:
- **`dcgm-exporter`** — NVIDIA's standard GPU metrics exporter (from the GPU Operator or the standalone chart).
- **Per-process GPU exporters** — anything that emits `gpu_process_memory_bytes` with `pod`/`namespace`/`container` labels. Used when workloads bypass the device plugin via `NVIDIA_VISIBLE_DEVICES=all` and dcgm-exporter can't attribute by itself.

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


<img width="1024" height="671" alt="image" src="https://github.com/user-attachments/assets/9b034b43-8dfe-4caa-b093-6092023d55ad" />

Rows are sorted by physical GPU, then by MIG slice ID. A blank line separates each physical card so pile-ups are obvious at a glance.

## Requirements

### On the cluster

- An NVIDIA GPU exporter the tool recognises:
  - **`dcgm-exporter`** — image name contains `dcgm-exporter`, or its `/metrics` emits `DCGM_FI_DEV_*` family names.
  - **A per-process GPU exporter** — anything whose `/metrics` emits `gpu_process_memory_bytes` with `pod`/`namespace`/`container` labels. Tools that produce this format work without code changes.
  - Use `--exporter` to point at any pod by `namespace/name:port` if image-name and metric-content auto-detection don't catch your setup.
- For **per-pod attribution** from dcgm-exporter alone: dcgm-exporter must run with `--kubernetes` enabled, so metrics carry `namespace`/`pod` labels. The NVIDIA GPU Operator does this by default. Without it, you fall back to per-(node, GPU) rows.
- For workloads using `NVIDIA_VISIBLE_DEVICES=all` to bypass the device plugin, dcgm-exporter can't attribute (kubelet's pod-resources API knows nothing). For that case you need a per-process exporter deployed alongside.
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

## Install

### Recommended: via Krew (the kubectl plugin manager)

If you already have [krew](https://krew.sigs.k8s.io/) installed, this is a one-liner:

```sh
kubectl krew install gpugo
kubectl gpugo
```

That's it. Krew downloads the right binary for your OS/arch, sha256-verifies it, drops it on `$PATH`, and `kubectl krew upgrade` keeps it current along with every other plugin.

#### Don't have krew yet?

**macOS (Homebrew):**

```sh
brew install krew
echo 'export PATH="${KREW_ROOT:-$HOME/.krew}/bin:$PATH"' >> ~/.zshrc
source ~/.zshrc
```

**macOS / Linux (no Homebrew):**

```sh
(
  set -x; cd "$(mktemp -d)" &&
  OS="$(uname | tr '[:upper:]' '[:lower:]')" &&
  ARCH="$(uname -m | sed -e 's/x86_64/amd64/' -e 's/\(arm\)\(64\)\?.*/\1\2/' -e 's/aarch64$/arm64/')" &&
  KREW="krew-${OS}_${ARCH}" &&
  curl -fsSLO "https://github.com/kubernetes-sigs/krew/releases/latest/download/${KREW}.tar.gz" &&
  tar zxvf "${KREW}.tar.gz" &&
  ./"${KREW}" install krew
)
echo 'export PATH="${KREW_ROOT:-$HOME/.krew}/bin:$PATH"' >> ~/.zshrc   # or ~/.bashrc
source ~/.zshrc
```

**Windows** has a [separate installer](https://krew.sigs.k8s.io/docs/user-guide/setup/install/).

Then run `kubectl krew install gpugo` from the recommended section above.

### Alternative: from source

```sh
go install github.com/Tal-Naeh/kubectl-gpugo@latest
# put the resulting binary on your PATH, renamed to `kubectl-gpugo`
mv "$(go env GOPATH)/bin/kubectl-gpugo" /usr/local/bin/kubectl-gpugo
kubectl gpugo
```

Useful if you want to track `main` instead of tagged releases, or your environment can't reach github.com/kubernetes-sigs/krew.

### Alternative: pre-built archive

Download the archive matching your OS/arch from the [releases page](https://github.com/Tal-Naeh/kubectl-gpugo/releases), extract `kubectl-gpugo`, and drop it anywhere on `$PATH`.

### How `kubectl` finds the plugin

`kubectl` discovers plugins by looking for any binary named `kubectl-<something>` on `$PATH`. The standard kubectl flags (`--context`, `--kubeconfig`, `-n`, etc.) are routed automatically via `genericclioptions` — they behave exactly like in vanilla `kubectl`.

## Flags

| Flag                        | Purpose                                                                                                  |
|-----------------------------|----------------------------------------------------------------------------------------------------------|
| `--kubeconfig`, `--context` | Standard kubectl flags                                                                                   |
| `-n`, `--namespace`         | Only show workload pods in this namespace (exporter discovery stays cluster-wide).                        |
| `--once`                    | Take one snapshot, print a plain table to stdout and exit. No TUI, no colour. Pipe-friendly.             |
| `-o`, `--output table\|json` | Output format for `--once`. `json` implies `--once`.                                                    |
| `--interval 20s`            | TUI refresh cadence (any Go duration, e.g. `5s`, `1m`).                                                   |
| `--exporter ns/pod:port`    | Skip auto-discovery and scrape a specific pod. Comma-separate or repeat the flag for multiple pods.      |
| `--dump`                    | Print raw `/metrics` from every auto-discovered exporter and exit. Useful for debugging label conventions. |
| `--dump-pod ns/pod:port`    | Print raw `/metrics` from one specific pod and exit.                                                     |
| `--version`                 | Print the version and exit.                                                                              |

## Scripting: `--once` and JSON

```sh
# plain table, e.g. for a cron job, a CI gate, or a k9s plugin pane
kubectl gpugo --once
kubectl gpugo --once -n ml

# machine-readable
kubectl gpugo -o json | jq '.rows[] | select(.gpuUtilPct < 5 and .vramUsedMiB > 10000) | "\(.namespace)/\(.pod)"'
```

JSON shape (fields are additive-only across versions):

```json
{
  "scrapedAt": "2026-09-08T09:12:44Z",
  "mode": "pod",
  "rows": [
    {
      "namespace": "ml", "pod": "vllm-0", "node": "node-a",
      "gpus": ["0", "1"], "gpuCount": 2,
      "gpuUtilPct": 65, "vramUsedMiB": 135000, "vramFreeMiB": 27000, "powerWatts": 511
    }
  ]
}
```

`mode` is `"pod"` when rows are attributed to workload pods and `"gpu"` when dcgm-exporter gave no pod labels; in that case each row carries `gpuIndex` and `hintPods` (pods on that node requesting `nvidia.com/gpu`).

### k9s plugin

Drop this into `~/.config/k9s/plugins.yaml` and press `Shift-G` on any pod view:

```yaml
plugins:
  gpugo:
    shortCut: Shift-G
    description: GPU usage (kubectl-gpugo)
    scopes: [pods]
    command: kubectl
    background: false
    args:
      - gpugo
      - --context
      - $CONTEXT
      - -n
      - $NAMESPACE
      - --once
```

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

## Prefer a GUI? Freelens extension

The same discovery, scraping and attribution logic ships as a Freelens extension: [`freelens-gpu-extension`](https://github.com/Tal-Naeh/freelens-gpu-extension). It adds a **GPU** page to the cluster sidebar and GPU sections to the Pod and Node detail drawers, and it works through Freelens' own cluster connection (so the Lens-proxy caveat above does not apply there). Install from *File → Extensions* with the npm name `@tal-naeh/freelens-gpu-extension`.

## Local dev

```sh
go build ./...
go test ./...          # parser/aggregation tests run against fixtures in internal/scraper/testdata
./kubectl-gpugo --interval 5s
```

Without a GPU cluster at hand you can run the whole thing against a fake exporter: any pod named like `*dcgm-exporter*` that serves a DCGM-style Prometheus text file on `/metrics` (e.g. nginx + a ConfigMap) is discovered and rendered exactly like the real DaemonSet. The fixtures under `internal/scraper/testdata/` are valid input.

CI (`.github/workflows/ci.yml`) runs gofmt, vet, tests with `-race`, and a cross-compile smoke test on every PR.

## Limitations / known issues

- The tool scrapes through the apiserver pod-proxy subresource. Slow control planes (e.g. RKE2 on a busy DGX with many MIG slices) can take 5–10s per scrape. If the apiserver gets wedged, restart the dcgm-exporter pod.
- Image-name classification is a short hardcoded list. Forked images with unusual names still get caught by the `/metrics` probe fallback, but if both the name AND the metric families look unusual the pod is skipped. Use `--exporter` as an override.
- Power on shared GPUs (multiple pods on one card via MIG or `NVIDIA_VISIBLE_DEVICES=all`) is attributed proportionally to each pod's VRAM share — the most honest split DCGM data supports.
