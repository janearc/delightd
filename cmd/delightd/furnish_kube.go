package main

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"delightd/pkg/kube"
)

// healthReader returns the live objects of one piece as kubeItems, so the health verb
// can judge readiness. Production builds the piece's kustomization and reads each
// object's live status via client-go; tests inject a fake that returns canned items.
// This replaces `kubectl get -k <piece> -o json`.
type healthReader func(ctx context.Context, pieceDir string) ([]kubeItem, error)

// objRef is one object a piece's kustomization declares: enough to look it up live.
type objRef struct {
	gvk       schema.GroupVersionKind
	name      string
	namespace string
}

// buildPiece renders a piece's kustomization in-process (no kubectl, no kustomize
// binary) and returns the objects it declares. It reads the desired set; the live
// status comes from the cluster.
func buildPiece(dir string) ([]objRef, error) {
	m, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("furnish: build %s: %w", dir, err)
	}
	refs := make([]objRef, 0, len(m.Resources()))
	for _, res := range m.Resources() {
		g := res.GetGvk()
		refs = append(refs, objRef{
			gvk:       schema.GroupVersionKind{Group: g.Group, Version: g.Version, Kind: g.Kind},
			name:      res.GetName(),
			namespace: res.GetNamespace(),
		})
	}
	return refs, nil
}

// kubeHealthReader is the production healthReader over a client-go client. Typed errors
// carry the taxonomy kubectl's text could not: a declared-but-absent object is a
// distinct state (reported not-ready, never GREEN-by-existence), while a transport /
// RBAC / timeout failure is returned as an error -- indeterminate, not "unhealthy".
func kubeHealthReader(c *kube.Client) healthReader {
	return func(ctx context.Context, pieceDir string) ([]kubeItem, error) {
		refs, err := buildPiece(pieceDir)
		if err != nil {
			return nil, err
		}
		items := make([]kubeItem, 0, len(refs))
		for _, ref := range refs {
			it, err := liveItem(ctx, c, ref)
			if err != nil {
				return nil, err
			}
			items = append(items, it)
		}
		return items, nil
	}
}

// liveItem reads one declared object's live state into a kubeItem.
func liveItem(ctx context.Context, c *kube.Client, ref objRef) (kubeItem, error) {
	mapping, err := c.Mapper.RESTMapping(ref.gvk.GroupKind(), ref.gvk.Version)
	if err != nil {
		return kubeItem{}, fmt.Errorf("furnish: resolve %s: %w", ref.gvk.Kind, err)
	}
	var ri dynamic.ResourceInterface = c.Dynamic.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := ref.namespace
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		ri = c.Dynamic.Resource(mapping.Resource).Namespace(ns)
	}

	it := kubeItem{Kind: ref.gvk.Kind}
	it.Metadata.Name = ref.name

	u, err := ri.Get(ctx, ref.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Declared but not deployed: absent, not a fault of the cluster or a false green.
		it.absent = true
		return it, nil
	}
	if err != nil {
		// Indeterminate (unreachable / RBAC / timeout), not "unhealthy".
		return kubeItem{}, fmt.Errorf("furnish: get %s/%s: %w", ref.gvk.Kind, ref.name, err)
	}
	if r, found, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); found {
		v := int32(r)
		it.Spec.Replicas = &v
	}
	if rr, found, _ := unstructured.NestedInt64(u.Object, "status", "readyReplicas"); found {
		it.Status.ReadyReplicas = int32(rr)
	}
	return it, nil
}
