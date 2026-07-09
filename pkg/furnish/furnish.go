// Package furnish converges the meubilair pieces -- the no-code kube deployments that
// live one directory per piece under a kube/ aggregator -- onto a cluster via client-go
// and the kustomize library. It is delightd's operator action over a *kube.Client, shared
// by the daemon's /furnish HTTP surface and the CLI so there is one implementation and one
// health taxonomy.
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
	for _, item := range items {
		state := "GREEN"
		detail := "present"
		switch {
		case item.Indeterminate:
			state = "INDETERMINATE"
			detail = item.Reason
			healthy = false
		case item.Absent:
			state = "RED"
			detail = "declared but absent"
			healthy = false
		case item.Kind == "Deployment" || item.Kind == "StatefulSet":
			want := int32(1)
			if item.Replicas != nil {
				want = *item.Replicas
			}
			if item.ReadyReplicas < want {
				state = "RED"
				healthy = false
			}
			detail = fmt.Sprintf("%d/%d ready", item.ReadyReplicas, want)
		}
		results = append(results, map[string]any{
			"kind": item.Kind, "name": item.Name, "state": state, "detail": detail,
		})
	}
	return healthy, results
}

// buildPiece renders a piece's kustomization in-process into the objects it declares.
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
// RESTMapper (a Deployment is namespaced; a ClusterRole is not). Resolution goes through
// kube.Client.RESTMapping, which invalidates the discovery cache once on a no-match, so a
// kind installed after the daemon's first discovery still resolves.
func resourceFor(c *kube.Client, u *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := u.GroupVersionKind()
	mapping, err := c.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("furnish: resolve %s: %w", gvk.Kind, err)
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := u.GetNamespace()
		if ns == "" {
			// Every piece declares its namespace (fleet) on every namespaced object; a manifest
			// that omits it is a bug in that manifest, not license to guess. Silently defaulting
			// to "default" would land the object somewhere no piece declares and no health check
			// looks -- fail loud and name the object instead.
			return nil, fmt.Errorf("furnish: %s/%s declares no namespace (every piece's namespaced objects must set one)", gvk.Kind, u.GetName())
		}
		return c.Dynamic.Resource(mapping.Resource).Namespace(ns), nil
	}
	return c.Dynamic.Resource(mapping.Resource), nil
}

// UpResult reports what Up actually touched: Applied names every object ("Kind/name") that
// server-side-apply succeeded on; Failed maps a failed object's "Kind/name" to its error. A
// failure partway through a piece must not discard the objects that already landed -- the
// caller (the POST /furnish/{piece}/up handler) reports both, never a bare {"applied":true}
// that would claim success for objects that never actually applied.
type UpResult struct {
	Applied []string
	Failed  map[string]string
}

// Up converges every object a piece declares via server-side apply -- the API-native
// equivalent of `kubectl apply -k`, idempotent and merge-based. It does not abort on the
// first failing object: every declared object gets an apply attempt, so one bad object (a
// field-manager conflict, an RBAC refusal) does not hide whether the rest of the piece
// landed. The returned error is non-nil whenever any object failed and wraps the first
// failure (so a caller checking for a specific cause, e.g. a cluster-side refusal via
// errors.As, still can); the full split lives in the returned UpResult regardless.
func Up(ctx context.Context, c *kube.Client, pieceDir string) (UpResult, error) {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return UpResult{}, err
	}
	result := UpResult{Failed: map[string]string{}}
	var firstErr error
	for _, u := range objs {
		key := fmt.Sprintf("%s/%s", u.GetKind(), u.GetName())
		ri, err := resourceFor(c, u)
		if err != nil {
			result.Failed[key] = err.Error()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := ri.Apply(ctx, u.GetName(), u, metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
			objErr := fmt.Errorf("furnish: apply %s: %w", key, err)
			result.Failed[key] = objErr.Error()
			if firstErr == nil {
				firstErr = objErr
			}
			continue
		}
		result.Applied = append(result.Applied, key)
	}
	if len(result.Failed) > 0 {
		return result, fmt.Errorf("furnish: %d of %d object(s) failed to apply in %s: %w", len(result.Failed), len(objs), pieceDir, firstErr)
	}
	return result, nil
}

// Down removes every object a piece declares; an already-absent object is success
// (idempotent, matching `kubectl delete -k --ignore-not-found`). Deletes cascade in the
// background, kubectl's default: without an explicit PropagationPolicy the apiserver
// falls back to the kind's own default, which can orphan a Deployment's ReplicaSets and
// Pods -- the object is gone, the workload keeps running, and "removed" would be a lie.
func Down(ctx context.Context, c *kube.Client, pieceDir string) error {
	objs, err := buildPiece(pieceDir)
	if err != nil {
		return err
	}
	cascade := metav1.DeletePropagationBackground
	for _, u := range objs {
		ri, err := resourceFor(c, u)
		if err != nil {
			return err
		}
		if err := ri.Delete(ctx, u.GetName(), metav1.DeleteOptions{PropagationPolicy: &cascade}); err != nil && !apierrors.IsNotFound(err) {
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
	item := Item{Kind: u.GetKind(), Name: u.GetName()}

	ri, err := resourceFor(c, u)
	if err != nil {
		item.Indeterminate = true
		item.Reason = err.Error()
		return item
	}
	live, err := ri.Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		item.Absent = true
		return item
	}
	if err != nil {
		item.Indeterminate = true
		item.Reason = fmt.Sprintf("get %s/%s: %v", u.GetKind(), u.GetName(), err)
		return item
	}
	if r, found, _ := unstructured.NestedInt64(live.Object, "spec", "replicas"); found {
		v := int32(r)
		item.Replicas = &v
	}
	if rr, found, _ := unstructured.NestedInt64(live.Object, "status", "readyReplicas"); found {
		item.ReadyReplicas = int32(rr)
	}
	return item
}
