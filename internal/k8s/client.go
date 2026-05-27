package k8s

import (
	"fmt"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewClient builds a clientset honouring the usual kubectl flags. When running
// inside a pod with no kubeconfig present, ToRESTConfig() falls through to the
// in-cluster ServiceAccount config automatically, so the same binary works as
// a CLI plugin or as a Job inside the cluster.
func NewClient(cfgFlags *genericclioptions.ConfigFlags) (kubernetes.Interface, *rest.Config, error) {
	restCfg, err := cfgFlags.ToRESTConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("build REST config: %w", err)
	}

	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("build clientset: %w", err)
	}
	return cs, restCfg, nil
}
