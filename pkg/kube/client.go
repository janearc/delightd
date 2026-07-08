// Package kube is delightd's programmatic handle on the cluster it operates, via
// client-go instead of shelling out to the kubectl binary. delightd is the operator
// (it drives the cluster), not a pod in it, so it loads a kubeconfig with clientcmd's
// standard rules rather than in-cluster config. See docs/kubernetes-access.md; this
// replaces the kubectl-by-subprocess path (sprint-11 ruling reversed 2026-07-08 once
// client-go v0.35 + kustomize/api held the protobuf v1.36.11 pin).
package kube

import (
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
