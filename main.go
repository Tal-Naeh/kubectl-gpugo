// kubectl-gpugo is a top-like TUI for per-pod GPU utilisation on a Kubernetes
// cluster. It discovers existing dcgm-exporter pods (no DaemonSet install,
// zero cluster footprint) and scrapes them through the kube-apiserver pod
// proxy subresource — no local port-forward, no firewall surprises.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tal-Naeh/kubectl-gpugo/internal/k8s"
	"github.com/Tal-Naeh/kubectl-gpugo/internal/scraper"
	"github.com/Tal-Naeh/kubectl-gpugo/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/pflag"
	"k8s.io/cli-runtime/pkg/genericclioptions"
)

func main() {
	// Honour the standard kubectl flags (--kubeconfig, --context, -n, etc.)
	// so the plugin behaves like every other `kubectl ...` subcommand.
	cfgFlags := genericclioptions.NewConfigFlags(true)
	cfgFlags.AddFlags(pflag.CommandLine)

	dump := pflag.Bool("dump", false, "print raw /metrics from each dcgm-exporter and exit (for label-convention debugging)")
	dumpPod := pflag.String("dump-pod", "", "print raw /metrics from an explicit pod and exit; format: namespace/pod-name:port")
	exporters := pflag.StringSlice("exporter", nil, "explicit exporter target(s) to scrape; bypasses auto-discovery. Format: namespace/pod-name:port. Comma-separated or repeat the flag.")
	pflag.Parse()

	client, restCfg, err := k8s.NewClient(cfgFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kubectl-gpugo: kube client: %v\n", err)
		os.Exit(1)
	}

	scr := scraper.New(client, restCfg)

	if len(*exporters) > 0 {
		parsed, err := parseExporterSpecs(*exporters)
		if err != nil {
			fmt.Fprintf(os.Stderr, "kubectl-gpugo: %v\n", err)
			os.Exit(1)
		}
		scr.SetExplicit(parsed)
	}

	if *dump {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := scr.Dump(ctx, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "kubectl-gpugo: dump: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *dumpPod != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := scr.DumpPod(ctx, *dumpPod, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "kubectl-gpugo: dump-pod: %v\n", err)
			os.Exit(1)
		}
		return
	}

	p := tea.NewProgram(tui.NewModel(scr), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "kubectl-gpugo: tui: %v\n", err)
		os.Exit(1)
	}
}

// parseExporterSpecs turns "namespace/pod:port" strings (one per --exporter
// flag occurrence, or comma-separated within one) into ExporterPod stubs.
// The Kind field is left unset; the scraper probes /metrics at first
// discovery to classify them as DCGM, enricher, or skip.
func parseExporterSpecs(specs []string) ([]k8s.ExporterPod, error) {
	out := make([]k8s.ExporterPod, 0, len(specs))
	for _, spec := range specs {
		parts := strings.SplitN(spec, "/", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("bad --exporter %q: expected namespace/pod:port", spec)
		}
		np := strings.SplitN(parts[1], ":", 2)
		if len(np) != 2 {
			return nil, fmt.Errorf("bad --exporter %q: expected namespace/pod:port", spec)
		}
		port, err := strconv.Atoi(np[1])
		if err != nil || port <= 0 {
			return nil, fmt.Errorf("bad --exporter %q: invalid port", spec)
		}
		out = append(out, k8s.ExporterPod{
			Namespace: parts[0],
			Name:      np[0],
			Port:      int32(port),
		})
	}
	return out, nil
}
