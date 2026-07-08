package main

import (
	"context"
	"fmt"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"delightd/pkg/kube"
)

// fieldManager is the server-side-apply owner delightd's furnish stamps on the objects
// it converges, so re-applies are a merge and drift is reconciled, not a blind replace.
const fieldManager = "delightd-furnish"

// cluster is furnish's handle on the live cluster: converge (apply), remove (delete), and
// read (health) one piece. It replaces the kubectl subprocess seam. Injectable so the
// verbs are tested against a fake dynamic client, no cluster required.
type cluster interface {
	apply(ctx context.Context, pieceDir string) error
	remove(ctx context.Context, pieceDir string) error
	health(ctx context.Context, pieceDir string) ([]kubeItem, error)
}

// kubeCluster is the production cluster over a client-go client, built lazily on first
// use so the command tree constructs even with no kubeconfig present.
type kubeCluster struct {
	once sync.Once
	c    *kube.Client
	err  error
}

func (k *kubeCluster) client() (*kube.Client, error) {
	k.once.Do(func() { k.c, k.err = kube.FromKubeconfig("") })
	return k.c, k.err
}

func (k *kubeCluster) apply(ctx context.Context, pieceDir string) error {
	c, err := k.client()
	if err != nil {
		return err
	}
	return applyPiece(ctx, c, pieceDir)
}

func (k *kubeCluster) remove(ctx context.Context, pieceDir string) error {
	c, err := k.client()
	if err != nil {
		return err
	}
	return deletePiece(ctx, c, pieceDir)
}

func (k *kubeCluster) health(ctx context.Context, pieceDir string) ([]kubeItem, error) {
	c, err := k.client()
	if err != nil {
		return nil, err
	}
	return readHealth(ctx, c, pieceDir)
}

// buildPiece renders a piece's kustomization in-process (no kubectl, no kustomize
// binary) into the objects it declares.
func buildPiece(dir string) ([]*unstructured.Unstructured, error) {
	m, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
	if err != nil {
		return nil, fmt.Errorf("furnish: build %s: %w", dir, err)
	}
	objs := make([]*unstructured.Unstructured, 0, len(m.Resources()))
	for _, res := range m.Resources() {
		obj, err := res.Map()
		if err != nil {
			return nil, fmt.Errorf("furnish: read object in %s: %w", dir, err)
		}
		objs = append(objs, &unstructured.Unstructured{Object: obj})
	}
	return objs, nil
}

// resourceFor resolves an object to its dynamic resource client, namespaced or not per
// the RESTMapper (a Deployment is namespaced; a ClusterRole is not).
func resourceFor(c *kube.Client, u *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := u.GroupVersionKind()
	mapping, err := c.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("furnish: resolve %s: %w", gvk.Kind, err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := u.GetNamespace()
		if ns == "" {
			ns = metav1.NamespaceDefault
		}
		return c.Dynamic.Resource(mapping.Resource).Namespace(ns), nil
	}
	return c.Dynamic.Resource(mapping.Resource), nil
}

// applyPiece converges every object a piece declares via server-side apply -- the
// API-native equivalent of `kubectl apply -k`, idempotent and merge-based.
func applyPiece(ctx context.Context, c *kube.Client, pieceDir string) error {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return err
	}
	for _, u := range objs {
		ri, err := resourceFor(c, u)
		if err != nil {
			return err
		}
		if _, err := ri.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			return fmt.Errorf("furnish: apply %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

// deletePiece removes every object a piece declares; an already-absent object is
// success (idempotent, matching `kubectl delete -k --ignore-not-found`).
func deletePiece(ctx context.Context, c *kube.Client, pieceDir string) error {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return err
	}
	for _, u := range objs {
		ri, err := resourceFor(c, u)
		if err != nil {
			return err
		}
		if err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("furnish: delete %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

// readHealth reads each declared object's live state into a kubeItem. Typed errors carry
// the taxonomy kubectl's text could not: a declared-but-absent object (NotFound) is a
// distinct RED state, never GREEN-by-existence; a transport / RBAC / timeout failure is
// returned as an error -- indeterminate, not "unhealthy".
func readHealth(ctx context.Context, c *kube.Client, pieceDir string) ([]kubeItem, error) {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return nil, err
	}
	items := make([]kubeItem, 0, len(objs))
	for _, u := range objs {
		it, err := liveItem(ctx, c, u)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}

// liveItem reads one declared object's live state.
func liveItem(ctx context.Context, c *kube.Client, u *unstructured.Unstructured) (kubeItem, error) {
	ri, err := resourceFor(c, u)
	if err != nil {
		return kubeItem{}, err
	}
	it := kubeItem{Kind: u.GetKind()}
	it.Metadata.Name = u.GetName()

	live, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		it.absent = true
		return it, nil
	}
	if err != nil {
		return kubeItem{}, fmt.Errorf("furnish: get %s/%s: %w", u.GetKind(), u.GetName(), err)
	}
	if r, found, _ := unstructured.NestedInt64(live.Object, "spec", "replicas"); found {
		v := int32(r)
		it.Spec.Replicas = &v
	}
	if rr, found, _ := unstructured.NestedInt64(live.Object, "status", "readyReplicas"); found {
		it.Status.ReadyReplicas = int32(rr)
	}
	return it, nil
}
