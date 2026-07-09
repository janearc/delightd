package furnish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"delightd/pkg/kube"
)

var (
	deploymentGVK  = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	deploymentGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	clusterRoleGVK = schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
)

// staticMapper maps the kinds the fixtures use to their resources, so the reader needs no
// live discovery. ClusterRole is mapped root-scoped so resourceFor's cluster-scoped branch
// is exercised; a ConfigMap is deliberately left unmapped so the resolution-failure branch
// is too.
func staticMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "apps", Version: "v1"},
		{Group: "rbac.authorization.k8s.io", Version: "v1"},
	})
	m.Add(deploymentGVK, meta.RESTScopeNamespace)
	m.Add(clusterRoleGVK, meta.RESTScopeRoot)
	return m
}

func fakeClient(objs ...runtime.Object) *kube.Client {
	return &kube.Client{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...),
		Mapper:  staticMapper(),
	}
}

// writePiece lays down a piece kustomization declaring one namespaced Deployment.
func writePiece(t *testing.T) string {
	return writeSinglePiece(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: delightd\n  namespace: fleet\nspec:\n  replicas: 1\n")
}

// writeSinglePiece lays down a piece declaring one object from the given manifest.
func writeSinglePiece(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "obj.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - obj.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeTwoPiece lays down a piece declaring two namespaced Deployments, so a test can fail
// one object's read and assert the other is still reported.
func writeTwoPiece(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dep := func(name string) string {
		return fmt.Sprintf("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: %s\n  namespace: fleet\nspec:\n  replicas: 1\n", name)
	}
	for _, n := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(dir, n+".yaml"), []byte(dep(n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - alpha.yaml\n  - beta.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeAggregator lays down a minimal kube/ fixture: an aggregator declaring the given
// pieces. The piece directories themselves are not created -- Pieces trusts the
// declaration, which is exactly the production contract.
func writeAggregator(t *testing.T, pieces ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := "resources:\n"
	for _, p := range pieces {
		body += "  - " + p + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

func liveDeployment(ready int32) *unstructured.Unstructured {
	return liveNamedDeployment("delightd", ready)
}

func liveNamedDeployment(name string, ready int32) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(deploymentGVK)
	u.SetName(name)
	u.SetNamespace("fleet")
	_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedField(u.Object, int64(ready), "status", "readyReplicas")
	return u
}

func liveClusterRole() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterRoleGVK)
	u.SetName("delightd-operator")
	return u
}

// item builds an Item for the PieceHealth tests without repeating the field boilerplate.
func item(kind, name string, replicas *int32, ready int32) Item {
	return Item{Kind: kind, Name: name, Replicas: replicas, ReadyReplicas: ready}
}

const configMapManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: orphan\n  namespace: fleet\n"
const clusterRoleManifest = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: delightd-operator\n"

func TestPieces_ReadsTheDeclaration(t *testing.T) {
	dir := writeAggregator(t, "alpha", "beta")
	got, err := Pieces(dir)
	if err != nil {
		t.Fatalf("Pieces: %v", err)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Pieces = %v, want %v", got, want)
	}
	if _, err := Pieces(t.TempDir()); err == nil {
		t.Error("Pieces on a dir with no aggregator: want error, got nil")
	}
	if _, err := Pieces(writeAggregator(t)); err == nil {
		t.Error("Pieces on an empty declaration: want error, got nil")
	}
}

func TestPieceHealth_Ladder(t *testing.T) {
	one, two := int32(1), int32(2)
	if ok, _ := PieceHealth([]Item{item("Deployment", "delightd", &one, 1), item("Service", "delightd", nil, 0)}); !ok {
		t.Error("1/1 Deployment + present Service should be healthy")
	}
	if ok, _ := PieceHealth([]Item{item("Deployment", "delightd", &two, 0)}); ok {
		t.Error("a Deployment short of its replicas should be RED")
	}
	if ok, _ := PieceHealth([]Item{item("StatefulSet", "kafka", &one, 0)}); ok {
		t.Error("an unready StatefulSet should be RED")
	}
	if ok, _ := PieceHealth([]Item{item("StatefulSet", "kafka", &one, 1)}); !ok {
		t.Error("a ready StatefulSet should be GREEN")
	}
	absent := item("Service", "delightd", nil, 0)
	absent.Absent = true
	if ok, _ := PieceHealth([]Item{absent}); ok {
		t.Error("a declared-but-absent object should be RED")
	}
	ind := item("Deployment", "delightd", &one, 1)
	ind.Indeterminate = true
	ind.Reason = "timeout"
	ok, ladder := PieceHealth([]Item{ind})
	if ok {
		t.Error("an indeterminate object should make the piece unhealthy")
	}
	if ladder[0]["state"] != "INDETERMINATE" {
		t.Errorf("state = %v, want INDETERMINATE (distinct from RED)", ladder[0]["state"])
	}
}

// TestHealth_Live builds the piece's kustomization in-process and reads the live
// Deployment status through a fake dynamic client -- the real client-go read path.
func TestHealth_Live(t *testing.T) {
	dir := writePiece(t)
	items, err := Health(context.Background(), fakeClient(liveDeployment(1)), dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if ok, _ := PieceHealth(items); !ok {
		t.Errorf("1/1 Deployment should be healthy: %+v", items[0])
	}
	items, err = Health(context.Background(), fakeClient(liveDeployment(0)), dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := PieceHealth(items); ok {
		t.Error("0/1 Deployment should be RED")
	}
}

// TestHealth_Absent: a declared object that is not live is NotFound -> Absent -> RED, not
// a false GREEN and not a hard error.
func TestHealth_Absent(t *testing.T) {
	items, err := Health(context.Background(), fakeClient(), writePiece(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 || !items[0].Absent {
		t.Fatalf("declared-but-absent object not marked Absent: %+v", items)
	}
	if ok, _ := PieceHealth(items); ok {
		t.Error("absent object should be RED")
	}
}

// TestHealth_Indeterminate: an object we cannot read (a transport failure) is
// INDETERMINATE, not Absent and not a hard error, and the piece is not reported healthy.
func TestHealth_Indeterminate(t *testing.T) {
	c := fakeClient()
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})
	items, err := Health(context.Background(), c, writePiece(t))
	if err != nil {
		t.Fatalf("a transport failure must not hard-error the read: %v", err)
	}
	if len(items) != 1 || !items[0].Indeterminate || items[0].Absent {
		t.Fatalf("transport failure should mark the object indeterminate (not absent): %+v", items)
	}
	if !strings.Contains(items[0].Reason, "connection refused") {
		t.Errorf("indeterminate reason should carry the cause: %q", items[0].Reason)
	}
	ok, ladder := PieceHealth(items)
	if ok {
		t.Error("a piece with an unread object must not report healthy (fail loud when degraded)")
	}
	if ladder[0]["state"] != "INDETERMINATE" {
		t.Errorf("state = %v, want INDETERMINATE (distinct from RED)", ladder[0]["state"])
	}
}

// TestHealth_OneBadObjectDoesNotBlindOthers: one object's read failure leaves the other
// objects' states intact.
func TestHealth_OneBadObjectDoesNotBlindOthers(t *testing.T) {
	c := fakeClient(liveNamedDeployment("beta", 1))
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "alpha" {
			return true, nil, fmt.Errorf("timeout")
		}
		return false, nil, nil // beta falls through to the tracker
	})
	items, err := Health(context.Background(), c, writeTwoPiece(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (both objects reported despite one failing)", len(items))
	}
	byName := map[string]Item{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if !byName["alpha"].Indeterminate {
		t.Errorf("alpha should be indeterminate: %+v", byName["alpha"])
	}
	if byName["beta"].Indeterminate || byName["beta"].ReadyReplicas != 1 {
		t.Errorf("beta should have been read despite alpha failing: %+v", byName["beta"])
	}
}

// TestHealth_ClusterScoped exercises resourceFor's cluster-scoped branch: a ClusterRole is
// not namespaced, and a present non-workload kind is GREEN by existence.
func TestHealth_ClusterScoped(t *testing.T) {
	items, err := Health(context.Background(), fakeClient(liveClusterRole()), writeSinglePiece(t, clusterRoleManifest))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 || items[0].Absent || items[0].Indeterminate {
		t.Fatalf("a present cluster-scoped object should read present: %+v", items)
	}
	if ok, _ := PieceHealth(items); !ok {
		t.Error("a present ClusterRole is GREEN by existence")
	}
}

// TestHealth_UnresolvableKindIsIndeterminate: a kind the RESTMapper cannot resolve is
// INDETERMINATE per-object, not a fatal read.
func TestHealth_UnresolvableKindIsIndeterminate(t *testing.T) {
	items, err := Health(context.Background(), fakeClient(), writeSinglePiece(t, configMapManifest))
	if err != nil {
		t.Fatalf("an unresolvable kind should be per-object indeterminate, not fatal: %v", err)
	}
	if len(items) != 1 || !items[0].Indeterminate {
		t.Fatalf("unmapped kind should be indeterminate: %+v", items)
	}
}

// TestHealth_UnrenderablePieceIsFatal: a kustomization that does not render has nothing to
// report against, so Health errors (the one fatal path).
func TestHealth_UnrenderablePieceIsFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - nope.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Health(context.Background(), fakeClient(), dir); err == nil {
		t.Error("an unrenderable piece must error, not report an empty-but-healthy piece")
	}
}

// TestUp converges a piece via server-side apply. The fake dynamic client does not
// implement SSA's create-on-apply, so we assert Up resolves the declared object and issues
// an apply (an ApplyPatchType patch) for it. The SSA merge itself is the apiserver's job.
func TestUp(t *testing.T) {
	c := fakeClient()
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	var applied []string
	fdc.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			t.Errorf("patch type = %v, want server-side apply", pa.GetPatchType())
		}
		applied = append(applied, pa.Name)
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(deploymentGVK)
		u.SetName(pa.Name)
		u.SetNamespace(pa.Namespace)
		return true, u, nil
	})
	if err := Up(context.Background(), c, writePiece(t)); err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 1 || applied[0] != "delightd" {
		t.Errorf("applied = %v, want [delightd]", applied)
	}
}

// TestUp_APIErrorPropagates: an apiserver error on apply is surfaced with the object named.
func TestUp_APIErrorPropagates(t *testing.T) {
	c := fakeClient()
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver said no")
	})
	err := Up(context.Background(), c, writePiece(t))
	if err == nil || !strings.Contains(err.Error(), "delightd") {
		t.Errorf("apply error should propagate and name the object: %v", err)
	}
}

// TestUpDown_UnresolvableKindErrors: up and down surface an unresolvable kind rather than
// silently skipping the object.
func TestUpDown_UnresolvableKindErrors(t *testing.T) {
	dir := writeSinglePiece(t, configMapManifest)
	c := fakeClient()
	if err := Up(context.Background(), c, dir); err == nil {
		t.Error("Up should surface an unresolvable kind, not silently skip it")
	}
	if err := Down(context.Background(), c, dir); err == nil {
		t.Error("Down should surface an unresolvable kind")
	}
}

// lateKindMapper models the discovery cache going stale: it answers NoKindMatch until
// Reset (the CRD "appears" on re-discovery), then delegates to the real mapper. It counts
// resets so a test can pin the invalidation to exactly one.
type lateKindMapper struct {
	meta.RESTMapper
	resets     int
	alwaysMiss bool // a kind that never appears, even after re-discovery
}

func (m *lateKindMapper) Reset() { m.resets++ }

func (m *lateKindMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	if m.resets == 0 || m.alwaysMiss {
		return nil, &meta.NoKindMatchError{GroupKind: gk, SearchedVersions: versions}
	}
	return m.RESTMapper.RESTMapping(gk, versions...)
}

// TestUp_LateKindResolvesAfterOneInvalidation: a kind unknown to the first discovery (a
// CRD installed out-of-band after the daemon's mapper cached) must trigger exactly one
// mapper invalidation + retry, after which furnish succeeds -- no restart, no busy
// re-discovery.
func TestUp_LateKindResolvesAfterOneInvalidation(t *testing.T) {
	c := fakeClient()
	m := &lateKindMapper{RESTMapper: staticMapper()}
	c.Mapper = m
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	var applied []string
	fdc.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		applied = append(applied, pa.Name)
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(deploymentGVK)
		u.SetName(pa.Name)
		u.SetNamespace(pa.Namespace)
		return true, u, nil
	})
	if err := Up(context.Background(), c, writePiece(t)); err != nil {
		t.Fatalf("up after a late-appearing kind: %v", err)
	}
	if m.resets != 1 {
		t.Errorf("mapper invalidations = %d, want exactly 1", m.resets)
	}
	if len(applied) != 1 || applied[0] != "delightd" {
		t.Errorf("applied = %v, want [delightd]", applied)
	}
}

// TestUp_MissAfterInvalidationIsFinal: a kind still unknown after re-discovery fails with
// the no-match surfaced -- one retry, never a loop.
func TestUp_MissAfterInvalidationIsFinal(t *testing.T) {
	c := fakeClient()
	m := &lateKindMapper{RESTMapper: staticMapper(), alwaysMiss: true}
	c.Mapper = m
	if err := Up(context.Background(), c, writePiece(t)); err == nil {
		t.Error("a kind absent even after re-discovery must still error")
	}
	if m.resets != 1 {
		t.Errorf("mapper invalidations = %d, want exactly 1 (retry once, never loop)", m.resets)
	}
}

// TestDown removes the declared object and treats an already-absent object as success.
func TestDown(t *testing.T) {
	dir := writePiece(t)
	c := fakeClient(liveDeployment(1))
	if err := Down(context.Background(), c, dir); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Dynamic.Resource(deploymentGVR).Namespace("fleet").Get(context.Background(), "delightd", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("object still present after delete: err=%v", err)
	}
	if err := Down(context.Background(), c, dir); err != nil {
		t.Errorf("delete of an absent piece should succeed: %v", err)
	}
}

// TestDown_CascadesInBackground: every delete must carry PropagationPolicy background --
// the kubectl-delete default this path replaced. An unset policy lets the apiserver fall
// back to the kind's default, which can orphan a Deployment's ReplicaSets and Pods while
// the handler reports removed:true.
func TestDown_CascadesInBackground(t *testing.T) {
	c := fakeClient(liveDeployment(1))
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	var policies []*metav1.DeletionPropagation
	fdc.PrependReactor("delete", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		policies = append(policies, a.(k8stesting.DeleteActionImpl).DeleteOptions.PropagationPolicy)
		return true, nil, nil
	})
	if err := Down(context.Background(), c, writePiece(t)); err != nil {
		t.Fatalf("down: %v", err)
	}
	if len(policies) != 1 {
		t.Fatalf("deletes = %d, want 1", len(policies))
	}
	if policies[0] == nil || *policies[0] != metav1.DeletePropagationBackground {
		t.Errorf("delete PropagationPolicy = %v, want Background (unset orphans workloads)", policies[0])
	}
}

// TestDown_APIErrorPropagates: a delete error that is not NotFound is surfaced.
func TestDown_APIErrorPropagates(t *testing.T) {
	c := fakeClient(liveDeployment(1))
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver said no")
	})
	if err := Down(context.Background(), c, writePiece(t)); err == nil {
		t.Error("a non-NotFound delete error should propagate")
	}
}
