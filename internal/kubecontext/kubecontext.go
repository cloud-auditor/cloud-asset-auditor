// Package kubecontext exposes the kubeconfig's context list without pulling
// in (and registering) the Kubernetes provider. The web server uses it to
// populate its context picker, and the provider uses it to expand the "all"
// sentinel — both need the same view of "which clusters can we reach", so it
// lives in one dependency-light place with no init() side effects.
package kubecontext

import (
	"os"
	"sort"

	"k8s.io/client-go/tools/clientcmd"
)

// List returns the context names defined in the resolved kubeconfig
// (KUBECONFIG env, else ~/.kube/config) plus the kubeconfig's current-context.
//
// A missing/empty kubeconfig — or running in-cluster, where a pod talks to
// exactly one API server — yields an empty list and no error. Callers treat
// "nothing to choose from" as a normal state, not a failure.
func List() (names []string, current string, err error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return nil, "", nil
	}

	cfg, err := clientcmd.NewDefaultClientConfigLoadingRules().Load()
	if err != nil {
		return nil, "", err
	}
	names = make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cfg.CurrentContext, nil
}
