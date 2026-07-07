// Package kube validates the kube/ manifest set: that the per-piece layout is
// intact, that the manifests decode as real, strictly-typed Kubernetes objects
// (not merely well-formed YAML), and that the aggregator renders a valid kube
// set through kustomize. It decodes into k8s.io/api's own Deployment/Service
// types with strict decoding on, so a misspelled field or wrong type on a
// core/apps kind is a hard error -- the same types the API server uses. CRDs
// (e.g. traefik IngressRoute) have no Go type here; they are validated as
// well-formed objects carrying apiVersion + kind, not strictly typed.
//
// This is test-only, types-only: k8s.io/api gives the object types for
// validation; nothing under cmd/ or pkg/ imports it, and the daemon still makes
// no Kubernetes API calls (no client-go).
package kube

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"delightd/config"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

// repoRoot walks up from this test's dir (test/kube) to the repository root.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(wd))
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// splitDocs breaks a possibly multi-document YAML stream (kustomize output) into
// its individual documents, using apimachinery's own reader.
func splitDocs(t *testing.T, data []byte) [][]byte {
	t.Helper()
	r := k8syaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	var docs [][]byte
	for {
		doc, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("split yaml: %v", err)
		}
		if len(bytes.TrimSpace(doc)) > 0 {
			docs = append(docs, doc)
		}
	}
	return docs
}

// decodeKube strictly decodes every document in data into typed Kubernetes
// objects. A parse error, an unknown/misspelled field, a wrong type, or an
// unregistered apiVersion/kind fails the test -- this is the "is it valid kube"
// gate, not a "does it look like yaml" check.
func decodeKube(t *testing.T, data []byte) []runtime.Object {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("register apps/v1: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("register core/v1: %v", err)
	}
	dec := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, scheme, scheme,
		kjson.SerializerOptions{Yaml: true, Strict: true})

	var out []runtime.Object
	for _, doc := range splitDocs(t, data) {
		obj, gvk, err := dec.Decode(doc, nil, nil)
		if err != nil {
			// CRDs (e.g. traefik IngressRoute, which grafana/kibana carry for edge
			// exposure) have no Go type in this test's core+apps scheme. The API
			// server accepts them because the CRD is installed; we cannot strictly
			// type them here without vendoring each CRD's module. Fall back to a
			// well-formedness check -- a valid document carrying apiVersion + kind --
			// so they are still validated, while every core/apps kind we register
			// stays strictly typed.
			if runtime.IsNotRegisteredError(err) {
				var tm struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
				}
				if ferr := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(doc), len(doc)).Decode(&tm); ferr != nil {
					t.Fatalf("unregistered kind %v; fallback decode failed: %v\n---\n%s", gvk, ferr, doc)
				}
				if tm.APIVersion == "" || tm.Kind == "" {
					t.Fatalf("unregistered object missing apiVersion/kind:\n---\n%s", doc)
				}
				continue
			}
			t.Fatalf("decode as valid kube object failed (gvk=%v): %v\n---\n%s", gvk, err, doc)
		}
		out = append(out, obj)
	}
	return out
}

func containerPort(c corev1.Container, name string) (int32, bool) {
	for _, p := range c.Ports {
		if p.Name == name {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

func servicePort(s *corev1.Service, name string) (int32, bool) {
	for _, p := range s.Spec.Ports {
		if p.Name == name {
			return p.Port, true
		}
	}
	return 0, false
}

// TestLayoutFilesPresent asserts delightd's manifests live under kube/delightd/
// and the pre-restructure flat paths are gone -- a partial revert that left one
// behind would build a duplicate delightd under the aggregator.
func TestLayoutFilesPresent(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range []string{
		"kube/kustomization.yaml",
		"kube/delightd/deployment.yaml",
		"kube/delightd/service.yaml",
		"kube/delightd/kustomization.yaml",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		}
	}
	for _, rel := range []string{"kube/deployment.yaml", "kube/service.yaml"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("stale flat manifest %s still present; it moved to kube/delightd/", rel)
		}
	}
}

// TestDelightdManifestsAreValidKube decodes the delightd manifests into their
// real typed objects and asserts the invariants that must hold on the wire: the
// fleet namespace, a container image, and the served control port matching the
// canonical DefaultControlPort the code defaults to.
func TestDelightdManifestsAreValidKube(t *testing.T) {
	root := repoRoot(t)

	objs := decodeKube(t, readFile(t, filepath.Join(root, "kube/delightd/deployment.yaml")))
	if len(objs) != 1 {
		t.Fatalf("deployment.yaml decoded to %d objects, want 1", len(objs))
	}
	d, ok := objs[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("deployment.yaml decoded to %T, want *appsv1.Deployment", objs[0])
	}
	if d.Namespace != "fleet" {
		t.Errorf("deployment namespace = %q, want \"fleet\"", d.Namespace)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("deployment has no containers")
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image == "" {
		t.Error("deployment container image is empty")
	}
	if port, found := containerPort(c, "control"); !found {
		t.Error("deployment has no container port named \"control\"")
	} else if int(port) != config.DefaultControlPort {
		t.Errorf("deployment control containerPort = %d, want DefaultControlPort %d", port, config.DefaultControlPort)
	}

	objs = decodeKube(t, readFile(t, filepath.Join(root, "kube/delightd/service.yaml")))
	if len(objs) != 1 {
		t.Fatalf("service.yaml decoded to %d objects, want 1", len(objs))
	}
	s, ok := objs[0].(*corev1.Service)
	if !ok {
		t.Fatalf("service.yaml decoded to %T, want *corev1.Service", objs[0])
	}
	if s.Namespace != "fleet" {
		t.Errorf("service namespace = %q, want \"fleet\"", s.Namespace)
	}
	if port, found := servicePort(s, "control"); !found {
		t.Error("service has no port named \"control\"")
	} else if int(port) != config.DefaultControlPort {
		t.Errorf("service control port = %d, want DefaultControlPort %d", port, config.DefaultControlPort)
	}
}

// TestAggregatorRendersValidKube runs the real kustomize build for the whole
// environment and for the delightd piece alone, and strictly decodes the
// rendered output. It proves the top-level kustomization actually aggregates
// delightd (the rendered set contains it) and that both builds produce valid
// kube -- not just that the files parse. Skips only where kubectl is absent.
func TestAggregatorRendersValidKube(t *testing.T) {
	root := repoRoot(t)
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl not on PATH; skipping kustomize render validation")
	}

	for _, target := range []string{"kube/", "kube/delightd/"} {
		cmd := exec.Command(kubectl, "kustomize", target)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "KUBECONFIG=/dev/null") // never contact a cluster
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("kubectl kustomize %s failed: %v\n%s", target, err, out)
		}

		var haveDeploy, haveSvc bool
		for _, obj := range decodeKube(t, out) {
			switch o := obj.(type) {
			case *appsv1.Deployment:
				if o.Name == "delightd" && o.Namespace == "fleet" {
					haveDeploy = true
				}
			case *corev1.Service:
				if o.Name == "delightd" && o.Namespace == "fleet" {
					haveSvc = true
				}
			}
		}
		if !haveDeploy {
			t.Errorf("kustomize %s did not render a delightd Deployment in fleet", target)
		}
		if !haveSvc {
			t.Errorf("kustomize %s did not render a delightd Service in fleet", target)
		}
	}
}
