package kubernetes

import (
	"fmt"
	"os"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// loadRESTConfig resolves the cluster connection, returning the REST config
// and a stable cluster identifier (used as Asset.AccountID).
//
// Order:
//  1. In-cluster — when KUBERNETES_SERVICE_HOST is set (we're a pod). The
//     "cluster ID" we record is just "in-cluster" because there's nothing
//     more authoritative available without an API call.
//  2. Kubeconfig — KUBECONFIG env first, then ~/.kube/config. When
//     contextName is non-empty it overrides the kubeconfig's current-context.
//     The cluster ID is the kubeconfig context's cluster field, which is
//     what `kubectl config get-contexts` shows.
func loadRESTConfig(contextName string) (*rest.Config, string, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return nil, "", fmt.Errorf("in-cluster config: %w", err)
		}
		return cfg, "in-cluster", nil
	}

	loader := clientcmd.NewDefaultClientConfigLoadingRules() // honors KUBECONFIG env + ~/.kube/config
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, overrides)
	cfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, "", fmt.Errorf("client config: %w", err)
	}

	clusterID := clusterIDFrom(clientConfig, contextName)
	return cfg, clusterID, nil
}

// clusterIDFrom extracts the cluster name from the resolved kubeconfig
// context. Falls back to the context name (or "unknown-cluster") when the
// raw config can't be loaded — neither matters for correctness, just for
// what shows up in Asset.AccountID.
func clusterIDFrom(cc clientcmd.ClientConfig, contextOverride string) string {
	raw, err := cc.RawConfig()
	if err != nil {
		if contextOverride != "" {
			return contextOverride
		}
		return "unknown-cluster"
	}
	ctxName := contextOverride
	if ctxName == "" {
		ctxName = raw.CurrentContext
	}
	if c, ok := raw.Contexts[ctxName]; ok && c.Cluster != "" {
		return c.Cluster
	}
	if ctxName != "" {
		return ctxName
	}
	return "unknown-cluster"
}
