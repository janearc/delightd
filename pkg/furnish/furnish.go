// Package furnish converges the meubilair pieces -- the no-code kube deployments that
// live one directory per piece under a kube/ aggregator -- onto a cluster via client-go
// and the kustomize library, with no kubectl or kustomize binary. It is delightd's
// operator action over a *kube.Client, shared by the daemon's /furnish HTTP surface and
// the CLI so there is one implementation and one health taxonomy.
//
// The taxonomy is deliberate: an object is GREEN when observed ready (or present, for
// non-workload kinds), RED when observed unhealthy or declared-but-absent, and
// INDETERMINATE when it could not be read at all. One unreadable object never aborts the
// read or blinds the rest of the piece; only a piece that will not render is fatal.
package furnish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"delightd/pkg/kube"
)

// fieldManager is the server-side-apply owner delightd's furnish stamps on the objects it
// converges, so re-applies are a merge and drift is reconciled, not a blind replace.
const fieldManager = "delightd-furnish"

// aggregator is the part of a kube/kustomization.yaml furnish reads: the resources list.
// A directory under kube/ is a piece only if it is named there -- the declaration, not
// the filesystem, says what exists.
type aggregator struct {
	Resources []string `yaml:"resources"`
}

// Pieces returns the declared pieces under kubeDir, in declaration order.
func Pieces(kubeDir string) ([]string, error) {
	path := filepath.Join(kubeDir, "kustomization.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("furnish: no aggregator at %s (point the manifest root at a checkout's kube/): %w", path, err)
	}
	var agg aggregator
	if err := yaml.Unmarshal(b, &agg); err != nil {
		return nil, fmt.Errorf("furnish: %s does not parse: %w", path, err)
	}
	if len(agg.Resources) == 0 {
		return nil, fmt.Errorf("furnish: %s declares no resources", path)
	}
	return agg.Resources, nil
}

// Item is one declared object's live health state. Replicas is the desired count (nil
// means kube's own default of 1). Absent, Indeterminate, and Reason carry the taxonomy:
// exactly one of {measured, Absent, Indeterminate} holds per object.
type Item struct {
	Kind          string
	Name          string
	Replicas      *int32
	ReadyReplicas int32
	// Absent: the object is declared but not live (NotFound). RED, never GREEN-by-existence.
	Absent bool
	// Indeterminate: the object could not be read at all (transport, RBAC, timeout, or an
	// unresolvable kind). Not an observation of health -- reported distinctly, never
	// GREEN (unknown is not healthy) and never RED (not observed to fail). Reason carries
	// the cause.
	Indeterminate bool
	Reason        string
}

// PieceHealth walks one piece's objects and reports a ladder: a Deployment or StatefulSet
// is GREEN when readyReplicas meets spec.replicas (unset means 1), RED otherwise; any
// other kind that exists is GREEN by existence; an unread object is INDETERMINATE. The
// piece is healthy only if every object is GREEN -- both RED and INDETERMINATE make it
// unhealthy (fail loud when degraded), but the two are labelled distinctly so a broken
// piece is told from an unreachable one. StatefulSet is gated like Deployment because the
// relocated bus/store pieces (kafka, zookeeper, elasticsearch) are StatefulSets: without
// it a CrashLooping kafka-0 would report GREEN by mere existence and health would lie.
func PieceHealth(items []Item) (bool, []map[string]any) {
	healthy := true
	var results []map[string]any
	for _, it := range items {
		state := "GREEN"
		detail := "present"
		switch {
		case it.Indeterminate:
			state = "INDETERMINATE"
			detail = it.Reason
			healthy = false
		case it.Absent:
			state = "RED"
			detail = "declared but absent"
			healthy = false
		case it.Kind == "Deployment" || it.Kind == "StatefulSet":
			want := int32(1)
			if it.Replicas != nil {
				want = *it.Replicas
			}
			if it.ReadyReplicas < want {
				state = "RED"
				healthy = false
			}
			detail = fmt.Sprintf("%d/%d ready", it.ReadyReplicas, want)
		}
		results = append(results, map[string]any{
			"kind": it.Kind, "name": it.Name, "state": state, "detail": detail,
		})
	}
	return healthy, results
}

// buildPiece renders a piece's kustomization in-process (no kubectl, no kustomize binary)
// into the objects it declares.
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

// resourceFor resolves an object to its dynamic resource client, namespaced or not per the
// RESTMapper (a Deployment is namespaced; a ClusterRole is not).
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

// Up converges every object a piece declares via server-side apply -- the API-native
// equivalent of `kubectl apply -k`, idempotent and merge-based.
func Up(ctx context.Context, c *kube.Client, pieceDir string) error {
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

// Down removes every object a piece declares; an already-absent object is success
// (idempotent, matching `kubectl delete -k --ignore-not-found`).
func Down(ctx context.Context, c *kube.Client, pieceDir string) error {
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

// Health reads each declared object's live state into an Item. One unreadable object does
// not abort the read -- every declared object gets a state, so a single flaky Get cannot
// blind the rest of the piece. Only a failure to render the piece (buildPiece) is fatal:
// with no rendered set there is nothing to report against.
func Health(ctx context.Context, c *kube.Client, pieceDir string) ([]Item, error) {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(objs))
	for _, u := range objs {
		items = append(items, liveItem(ctx, c, u))
	}
	return items, nil
}

// liveItem reads one declared object's live state. It never returns an error: a per-object
// read failure becomes an INDETERMINATE item carrying the cause, so the caller can keep
// reading the rest of the piece instead of losing every other object's state to one.
func liveItem(ctx context.Context, c *kube.Client, u *unstructured.Unstructured) Item {
	it := Item{Kind: u.GetKind(), Name: u.GetName()}

	ri, err := resourceFor(c, u)
	if err != nil {
		it.Indeterminate = true
		it.Reason = err.Error()
		return it
	}
	live, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		it.Absent = true
		return it
	}
	if err != nil {
		it.Indeterminate = true
		it.Reason = fmt.Sprintf("get %s/%s: %v", u.GetKind(), u.GetName(), err)
		return it
	}
	if r, found, _ := unstructured.NestedInt64(live.Object, "spec", "replicas"); found {
		v := int32(r)
		it.Replicas = &v
	}
	if rr, found, _ := unstructured.NestedInt64(live.Object, "status", "readyReplicas"); found {
		it.ReadyReplicas = int32(rr)
	}
	return it
}
