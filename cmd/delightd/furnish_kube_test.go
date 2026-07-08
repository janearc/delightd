package main

import (
	"context"
	"os"
	"path/filepath"
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
	deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	deploymentGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
)

// staticMapper maps the kinds the fixtures use to their resources, so the reader needs
// no live discovery in the test.
func staticMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	m.Add(deploymentGVK, meta.RESTScopeNamespace)
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
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(deploymentGVK)
	u.SetName("delightd")
	u.SetNamespace("fleet")
	_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedField(u.Object, int64(ready), "status", "readyReplicas")
	return u
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
