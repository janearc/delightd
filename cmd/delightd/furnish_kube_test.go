package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"delightd/pkg/kube"
)

var deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

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

// TestKubeHealthReader_Live builds the piece's kustomization in-process and reads the
// live Deployment status through a fake dynamic client -- the real client-go read path.
func TestKubeHealthReader_Live(t *testing.T) {
	dir := writePiece(t)

	// 1/1 -> healthy.
	items, err := kubeHealthReader(fakeClient(liveDeployment(1)))(context.Background(), dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if ok, _ := pieceHealth(items); !ok {
		t.Errorf("1/1 Deployment should be healthy: %+v", items[0])
	}

	// 0/1 -> RED.
	items, err = kubeHealthReader(fakeClient(liveDeployment(0)))(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := pieceHealth(items); ok {
		t.Error("0/1 Deployment should be RED")
	}
}

// TestKubeHealthReader_Absent: a declared object that is not live is NotFound -> absent
// -> RED, not a false GREEN and not a hard error.
func TestKubeHealthReader_Absent(t *testing.T) {
	dir := writePiece(t)
	items, err := kubeHealthReader(fakeClient())(context.Background(), dir) // nothing seeded
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
