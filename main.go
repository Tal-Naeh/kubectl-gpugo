// kubectl-gpugo is a top-like TUI for per-pod GPU utilisation on a Kubernetes
// cluster. It discovers existing dcgm-exporter pods (no DaemonSet install,
// zero cluster footprint) and scrapes them through the kube-apiserver pod
// proxy subresource — no local port-forward, no firewall surprises.
package main

import (
	"fmt"
	"os"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/k8s"
	"github.com/Tal-Naeh/kubectl-gpugo/internal/scraper"
	"github.com/Tal-Naeh/kubectl-gpugo/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func main() {
	// Honour the standard kubectl flags (--kubeconfig, --context, -n, etc.)
	// so the plugin behaves like every other `kubectl ...` subcommand.
	cfgFlags := genericclioptions.NewConfigFlags(true)

	client, restCfg, err := k8s.NewClient(cfgFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl-gpugo: kube client: %v\n", err)
		os.Exit(1)
	}

	scr := scraper.New(client, restCfg)

	p := tea.NewProgram(tui.NewModel(scr), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "kubectl-gpugo: tui: %v\n", err)
		os.Exit(1)
	}
}
