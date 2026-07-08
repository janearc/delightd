package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	clusterRoleGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
)

// staticMapper maps the kinds the fixtures use to their resources, so the reader needs
// no live discovery in the test. ClusterRole is mapped root-scoped so the cluster-scoped
// branch of resourceFor is exercised; a ConfigMap is deliberately left unmapped so the
// resolution-failure branch is exercised too.
func staticMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "apps", Version: "v1"},
		{Group: "rbac.authorization.k8s.io", Version: "v1"},
	})
	m.Add(deploymentGVK, meta.RESTScopeNamespace)
	m.Add(clusterRoleGVK, meta.RESTScopeRoot)
	return m
}

// writePiece lays down a piece kustomization declaring one namespaced Deployment.
func writePiece(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dep := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: delightd
  namespace: fleet
spec:
  replicas: 1
`
	if err := os.WriteFile(filepath.Join(dir, "deployment.yaml"), []byte(dep), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte("resources:\n  - deployment.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
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

// writeTwoPiece lays down a piece declaring two namespaced Deployments, so a test can
// fail one object's read and assert the other is still reported.
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
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"),
		[]byte("resources:\n  - alpha.yaml\n  - beta.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeClient(objs ...runtime.Object) *kube.Client {
	return &kube.Client{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...),
		Mapper:  staticMapper(),
	}
}

func getDeployment(t *testing.T, c *kube.Client) (*unstructured.Unstructured, error) {
	t.Helper()
	return c.Dynamic.Resource(deploymentGVR).Namespace("fleet").Get(context.Background(), "delightd", metav1.GetOptions{})
}

// TestReadHealth_Live builds the piece's kustomization in-process and reads the live
// Deployment status through a fake dynamic client -- the real client-go read path.
func TestReadHealth_Live(t *testing.T) {
	dir := writePiece(t)

	items, err := readHealth(context.Background(), fakeClient(liveDeployment(1)), dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if ok, _ := pieceHealth(items); !ok {
		t.Errorf("1/1 Deployment should be healthy: %+v", items[0])
	}

	items, err = readHealth(context.Background(), fakeClient(liveDeployment(0)), dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := pieceHealth(items); ok {
		t.Error("0/1 Deployment should be RED")
	}
}

// TestReadHealth_Absent: a declared object that is not live is NotFound -> absent -> RED,
// not a false GREEN and not a hard error.
func TestReadHealth_Absent(t *testing.T) {
	items, err := readHealth(context.Background(), fakeClient(), writePiece(t)) // nothing seeded
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 || !items[0].absent {
		t.Fatalf("declared-but-absent object not marked absent: %+v", items)
	}
	if ok, _ := pieceHealth(items); ok {
		t.Error("absent object should be RED")
	}
}

// TestReadHealth_Indeterminate: an object we cannot read (a transport failure here) is
// INDETERMINATE, not absent and not a hard error, and the piece is not reported healthy
// -- unknown is never presented as truth. Distinct from RED so the operator sees "could
// not reach it", not "it is broken".
func TestReadHealth_Indeterminate(t *testing.T) {
	dir := writePiece(t)
	c := fakeClient() // nothing seeded; the reactor turns the Get into a transport error
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("connection refused")
	})

	items, err := readHealth(context.Background(), c, dir)
	if err != nil {
		t.Fatalf("a transport failure must not hard-error the read: %v", err)
	}
	if len(items) != 1 || !items[0].indeterminate || items[0].absent {
		t.Fatalf("transport failure should mark the object indeterminate (not absent): %+v", items)
	}
	if !strings.Contains(items[0].reason, "connection refused") {
		t.Errorf("indeterminate reason should carry the cause: %q", items[0].reason)
	}
	ok, ladder := pieceHealth(items)
	if ok {
		t.Error("a piece with an unread object must not report healthy (fail loud when degraded)")
	}
	if ladder[0]["state"] != "INDETERMINATE" {
		t.Errorf("state = %v, want INDETERMINATE (distinct from RED)", ladder[0]["state"])
	}
}

// TestReadHealth_OneBadObjectDoesNotBlindOthers: one object's read failure leaves the
// other objects' states intact -- a single flaky Get cannot blind the whole piece.
func TestReadHealth_OneBadObjectDoesNotBlindOthers(t *testing.T) {
	dir := writeTwoPiece(t)
	c := fakeClient(liveNamedDeployment("beta", 1)) // beta is live and ready
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("get", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.(k8stesting.GetAction).GetName() == "alpha" {
			return true, nil, fmt.Errorf("timeout")
		}
		return false, nil, nil // beta falls through to the tracker
	})

	items, err := readHealth(context.Background(), c, dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (both objects reported despite one failing)", len(items))
	}
	byName := map[string]kubeItem{}
	for _, it := range items {
		byName[it.Metadata.Name] = it
	}
	if !byName["alpha"].indeterminate {
		t.Errorf("alpha should be indeterminate: %+v", byName["alpha"])
	}
	if byName["beta"].indeterminate || byName["beta"].Status.ReadyReplicas != 1 {
		t.Errorf("beta should have been read despite alpha failing: %+v", byName["beta"])
	}
}

// writeSinglePiece lays down a piece declaring one object from the given manifest.
func writeSinglePiece(t *testing.T, manifest string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "obj.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"),
		[]byte("resources:\n  - obj.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func liveClusterRole() *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(clusterRoleGVK)
	u.SetName("delightd-operator")
	return u
}

const configMapManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: orphan\n  namespace: fleet\n"
const clusterRoleManifest = "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: delightd-operator\n"

// TestReadHealth_ClusterScoped exercises resourceFor's cluster-scoped branch: a
// ClusterRole is not namespaced, and a present non-workload kind is GREEN by existence.
func TestReadHealth_ClusterScoped(t *testing.T) {
	dir := writeSinglePiece(t, clusterRoleManifest)
	items, err := readHealth(context.Background(), fakeClient(liveClusterRole()), dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 || items[0].absent || items[0].indeterminate {
		t.Fatalf("a present cluster-scoped object should read present: %+v", items)
	}
	if ok, _ := pieceHealth(items); !ok {
		t.Error("a present ClusterRole is GREEN by existence")
	}
}

// TestReadHealth_UnresolvableKindIsIndeterminate: a kind the RESTMapper cannot resolve
// is INDETERMINATE per-object, not a fatal read -- we could not even address it.
func TestReadHealth_UnresolvableKindIsIndeterminate(t *testing.T) {
	items, err := readHealth(context.Background(), fakeClient(), writeSinglePiece(t, configMapManifest))
	if err != nil {
		t.Fatalf("an unresolvable kind should be per-object indeterminate, not fatal: %v", err)
	}
	if len(items) != 1 || !items[0].indeterminate {
		t.Fatalf("unmapped kind should be indeterminate: %+v", items)
	}
}

// TestApplyDelete_UnresolvableKindErrors: apply and delete surface an unresolvable kind
// rather than silently skipping the object.
func TestApplyDelete_UnresolvableKindErrors(t *testing.T) {
	dir := writeSinglePiece(t, configMapManifest)
	c := fakeClient()
	if err := applyPiece(context.Background(), c, dir); err == nil {
		t.Error("applyPiece should surface an unresolvable kind, not silently skip it")
	}
	if err := deletePiece(context.Background(), c, dir); err == nil {
		t.Error("deletePiece should surface an unresolvable kind")
	}
}

// TestReadHealth_UnrenderablePieceIsFatal: a kustomization that does not render has
// nothing to report against, so readHealth errors (the one fatal path, distinct from a
// per-object read failure).
func TestReadHealth_UnrenderablePieceIsFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"),
		[]byte("resources:\n  - nope.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readHealth(context.Background(), fakeClient(), dir); err == nil {
		t.Error("an unrenderable piece must error, not report an empty-but-healthy piece")
	}
}

// TestApplyPiece converges a piece via server-side apply. The fake dynamic client does
// not implement SSA's create-on-apply, so we assert applyPiece resolves the declared
// object and issues an apply (an ApplyPatchType patch) for it -- the apply logic. The
// SSA merge itself is the apiserver's job, exercised against a live cluster.
func TestApplyPiece(t *testing.T) {
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

	if err := applyPiece(context.Background(), c, writePiece(t)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(applied) != 1 || applied[0] != "delightd" {
		t.Errorf("applied = %v, want [delightd]", applied)
	}
}

// TestApplyPiece_APIErrorPropagates: an apiserver error on apply is surfaced with the
// object named, not swallowed.
func TestApplyPiece_APIErrorPropagates(t *testing.T) {
	c := fakeClient()
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver said no")
	})
	err := applyPiece(context.Background(), c, writePiece(t))
	if err == nil || !strings.Contains(err.Error(), "delightd") {
		t.Errorf("apply error should propagate and name the object: %v", err)
	}
}

// TestDeletePiece_APIErrorPropagates: a delete error that is not NotFound is surfaced.
func TestDeletePiece_APIErrorPropagates(t *testing.T) {
	c := fakeClient(liveDeployment(1))
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	fdc.PrependReactor("delete", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("apiserver said no")
	})
	if err := deletePiece(context.Background(), c, writePiece(t)); err == nil {
		t.Error("a non-NotFound delete error should propagate")
	}
}

// TestDeletePiece removes the declared object and treats an already-absent object as
// success (idempotent).
func TestDeletePiece(t *testing.T) {
	dir := writePiece(t)
	c := fakeClient(liveDeployment(1))

	if err := deletePiece(context.Background(), c, dir); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := getDeployment(t, c); !apierrors.IsNotFound(err) {
		t.Errorf("object still present after delete: err=%v", err)
	}
	// Idempotent: deleting an already-absent piece is success.
	if err := deletePiece(context.Background(), c, dir); err != nil {
		t.Errorf("delete of an absent piece should succeed: %v", err)
	}
}
