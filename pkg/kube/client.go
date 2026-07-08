// Package kube is delightd's programmatic handle on the cluster it operates, via
// client-go instead of shelling out to the kubectl binary. delightd is the operator
// (it drives the cluster), not a pod in it, so it loads a kubeconfig with clientcmd's
// standard rules rather than in-cluster config. See docs/kubernetes-access.md; this
// replaces the kubectl-by-subprocess path (sprint-11 ruling reversed 2026-07-08 once
// client-go v0.35 + kustomize/api held the protobuf v1.36.11 pin).
package kube

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles a dynamic client (for arbitrary objects, the same generality kubectl
// had) with a RESTMapper that resolves a kind to its resource. The mapper is deferred:
// constructing it touches no network; it queries discovery only on first use.
type Client struct {
	Config  *rest.Config
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
}

// FromKubeconfig builds a Client from a kubeconfig. An empty path uses the standard
// loading rules ($KUBECONFIG, then ~/.kube/config), matching what the kubectl subprocess
// resolved -- furnish converges whatever cluster the operator's environment points at.
func FromKubeconfig(path string) (*Client, error) {
	cfg, err := RESTConfig(path)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{
		Config:  cfg,
		Dynamic: dyn,
		Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}, nil
}

// RESTConfig loads a *rest.Config from a kubeconfig using clientcmd's standard rules.
// Split out so it is testable against a fixture kubeconfig without a live cluster, and
// reusable by callers that need only the config (e.g. an apiserver readiness ping).
func RESTConfig(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// APIServerReady pings the apiserver's own /readyz endpoint via client-go, returning
// nil only when the server answers 2xx. It is the readiness probe's cluster check: the
// programmatic equivalent of kubectl's `get --raw=/readyz`, with no kubectl binary. The
// kubeconfig is resolved by the standard rules when path is empty, at call time, so
// startup never depends on a kubeconfig being present and one mounted later is picked
// up. The caller's context bounds the call, so a dead apiserver fails fast rather than
// hanging the probe.
func APIServerReady(ctx context.Context, path string) error {
	cfg, err := RESTConfig(path)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build discovery client: %w", err)
	}
	// AbsPath escapes the /api prefix to hit the apiserver's own /readyz -- the same
	// endpoint kubectl's --raw fetched. Do().Error() is nil only on a 2xx status.
	if err := dc.RESTClient().Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return fmt.Errorf("apiserver /readyz: %w", err)
	}
	return nil
}
