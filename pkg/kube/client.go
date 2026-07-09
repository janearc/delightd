// Package kube is delightd's programmatic handle on the cluster it operates, via
// client-go. delightd is the operator (it drives the cluster), not a pod in it, so it
// loads a kubeconfig with clientcmd's standard rules rather than in-cluster config. See
// docs/kubernetes-access.md.
package kube

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
)

// Client bundles a dynamic client (for arbitrary objects) with a RESTMapper that resolves
// a kind to its resource. The mapper is deferred: constructing it touches no network; it
// queries discovery only on first use.
type Client struct {
	Config  *rest.Config
	Dynamic dynamic.Interface
	Mapper  meta.RESTMapper
}

// FromKubeconfig builds a Client from a kubeconfig. An empty path uses the standard
// loading rules ($KUBECONFIG, then ~/.kube/config), so furnish converges whatever cluster
// the operator's environment points at.
func FromKubeconfig(path string) (*Client, error) {
	cfg, err := RESTConfig(path)
	if err != nil {
		return nil, err // RESTConfig already names the resolution it tried
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build dynamic client for %s: %w", cfg.Host, err)
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("kube: build discovery client for %s: %w", cfg.Host, err)
	}
	return &Client{
		Config:  cfg,
		Dynamic: dyn,
		Mapper:  restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc)),
	}, nil
}

// RESTMapping resolves a GroupKind to its mapping, and on a no-match invalidates the
// mapper's discovery cache once and retries -- the standard kubectl behavior. The daemon
// holds one Client for its whole life, and the memory-cached discovery behind the deferred
// mapper is fresh forever after first use; without the invalidation, a kind installed
// after first discovery (a CRD landing out-of-band during bring-up) stays NoKindMatch
// until restart. Exactly one invalidation per miss, and only on a miss -- an unknown kind
// costs one re-discovery, a known kind costs nothing, so there is no busy re-discovery on
// the hot path.
func (c *Client) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	mapping, err := c.Mapper.RESTMapping(gk, versions...)
	if err == nil || !meta.IsNoMatchError(err) {
		return mapping, err
	}
	// Only a resettable mapper (the deferred discovery mapper is; the tests' static
	// mappers are not) can refresh -- otherwise the miss is final.
	resettable, ok := c.Mapper.(meta.ResettableRESTMapper)
	if !ok {
		return mapping, err
	}
	resettable.Reset()
	return c.Mapper.RESTMapping(gk, versions...)
}

// LazyClient returns a provider that builds a *Client on first call from the standard
// kubeconfig rules and memoizes the success -- never a failure. A daemon can therefore
// start with no kubeconfig present and pick up a late-mounted one on a later call, while a
// success is built once and shared (delightd holds a single cluster handle, not one per
// request). Safe for concurrent use.
func LazyClient() func() (*Client, error) {
	var (
		mu sync.Mutex
		c  *Client
	)
	return func() (*Client, error) {
		mu.Lock()
		defer mu.Unlock()
		if c != nil {
			return c, nil
		}
		built, err := FromKubeconfig("")
		if err != nil {
			return nil, err // not memoized: a kubeconfig mounted later still resolves
		}
		c = built
		return c, nil
	}
}

// RESTConfig loads a *rest.Config from a kubeconfig using clientcmd's standard rules.
// Split out so it is testable against a fixture kubeconfig without a live cluster, and
// reusable by callers that need only the config (e.g. an apiserver readiness ping). The
// error names the resolution that was attempted so a failure points at the fix (a wrong
// --kubeconfig path, an unset $KUBECONFIG, a missing ~/.kube/config).
func RESTConfig(path string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if path != "" {
		rules.ExplicitPath = path
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		where := "$KUBECONFIG, then ~/.kube/config"
		if path != "" {
			where = fmt.Sprintf("path %q", path)
		}
		return nil, fmt.Errorf("kube: load kubeconfig (%s): %w", where, err)
	}
	return cfg, nil
}

// APIServerReady pings the apiserver's own /readyz endpoint via client-go, returning nil
// only when the server answers 2xx -- the programmatic equivalent of
// `kubectl get --raw=/readyz`. The kubeconfig is resolved by the standard rules when path
// is empty, at call time, so startup never depends on a kubeconfig being present and one
// mounted later is picked up. The caller's context bounds the call, so a dead apiserver
// fails fast rather than hanging the probe.
func APIServerReady(ctx context.Context, path string) error {
	cfg, err := RESTConfig(path)
	if err != nil {
		return err // RESTConfig already names the resolution it tried
	}
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("kube: build discovery client for %s: %w", cfg.Host, err)
	}
	// AbsPath escapes the /api prefix to hit the apiserver's own /readyz. Do().Error() is
	// nil only on a 2xx status.
	if err := dc.RESTClient().Get().AbsPath("/readyz").Do(ctx).Error(); err != nil {
		return fmt.Errorf("apiserver /readyz: %w", err)
	}
	return nil
}
