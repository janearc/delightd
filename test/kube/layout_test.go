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
// validation, and nothing under cmd/ or pkg/ imports this test package. The
// render below uses kustomize's library in-process (as furnish does), so the
// validation needs no kubectl or kustomize binary.
package kube

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
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

// toleratedCRDGVKs are the exact CRD group/version/kind triples this test
// validates for well-formedness only -- no Go type is registered for them.
// Keying on the full GVK means a typo in the group, version, OR kind of an
// otherwise-tolerated CRD hard-fails. traefik.io/v1alpha1/IngressRoute is the
// grafana/kibana edge route carried by the relocated furniture. Add a triple
// only when its CRD is actually furnished.
var toleratedCRDGVKs = map[string]bool{"traefik.io/v1alpha1/IngressRoute": true}

// kubeScheme registers the core + apps types once and is shared across
// decodeKube calls (the registration is identical every time). A registration
// failure is a programming error, so it panics at init rather than taking a *T.
var kubeScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := appsv1.AddToScheme(s); err != nil {
		panic("register apps/v1: " + err.Error())
	}
	if err := corev1.AddToScheme(s); err != nil {
		panic("register core/v1: " + err.Error())
	}
	return s
}()

// isCommentOnly reports whether a YAML document carries no content -- every
// non-empty line is a comment. Such a doc (a file's leading header before the
// first `---`) has no kind and is skipped by the decoder; a doc with real
// content is not comment-only and still gets strictly typed.
func isCommentOnly(doc []byte) bool {
	for line := range bytes.SplitSeq(doc, []byte("\n")) {
		s := bytes.TrimSpace(line)
		if len(s) > 0 && s[0] != '#' {
			return false
		}
	}
	return true
}

// decodeKube strictly decodes every resource document in data into typed
// Kubernetes objects (a parse error, misspelled field, wrong type, or an
// unregistered non-allowlisted kind fails the test) and returns the decoded
// core/apps objects plus the number of real (non-comment) documents it saw, so
// callers need not re-split. This is the "is it valid kube" gate, not a "does it
// look like yaml" check.
func decodeKube(t *testing.T, data []byte) ([]runtime.Object, int) {
	t.Helper()
	dec := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, kubeScheme, kubeScheme,
		kjson.SerializerOptions{Yaml: true, Strict: true})

	var out []runtime.Object
	realDocs := 0
	for i, doc := range splitDocs(t, data) {
		if isCommentOnly(doc) {
			// A leading comment-only document is a file header (before the first
			// `---`) -- skip it. A comment-only document ANYWHERE ELSE is a
			// resource that got commented out; fail, as the pre-tolerance decoder
			// did, so a half-commented multi-doc manifest cannot pass green.
			if i == 0 {
				continue
			}
			t.Fatalf("comment-only document at position %d (a commented-out resource?):\n---\n%s", i, doc)
		}
		realDocs++
		obj, gvk, err := dec.Decode(doc, nil, nil)
		if err != nil {
			// CRDs (e.g. traefik IngressRoute) have no Go type in the core+apps
			// scheme; we cannot strictly type them without vendoring each CRD's
			// module. On a NotRegistered error the serializer has already parsed
			// apiVersion+kind, so a non-nil gvk is proof of well-formedness. Allow
			// only the exact GVKs we furnish; any other unregistered kind -- a
			// typo'd core/apps kind, a typo'd version, group, or CRD kind --
			// hard-fails.
			if runtime.IsNotRegisteredError(err) {
				if gvk == nil || !toleratedCRDGVKs[gvk.Group+"/"+gvk.Version+"/"+gvk.Kind] {
					t.Fatalf("unregistered kind gvk=%v (not an allowlisted CRD; likely a typo): %v\n---\n%s", gvk, err, doc)
				}
				continue
			}
			t.Fatalf("decode as valid kube object failed (gvk=%v): %v\n---\n%s", gvk, err, doc)
		}
		out = append(out, obj)
	}
	return out, realDocs
}

// TestLayoutFilesPresent asserts the aggregator exists and that no delightd
// manifests do -- neither the pre-restructure flat paths nor kube/delightd/.
// delightd is the operator, not a piece: it runs as a host-level container,
// never a pod, and is never supervised by the fleet it operates. A revert that
// brings kube/delightd/ back would make the default `furnish health` RED by
// construction (the operator never exists in-cluster) and reintroduce the
// in-cluster delightd model docs/kubernetes-access.md records as rejected.
func TestLayoutFilesPresent(t *testing.T) {
	root := repoRoot(t)

	if _, err := os.Stat(filepath.Join(root, "kube/kustomization.yaml")); err != nil {
		t.Errorf("expected kube/kustomization.yaml to exist: %v", err)
	}
	for _, rel := range []string{"kube/deployment.yaml", "kube/service.yaml"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("stale flat manifest %s still present; delightd has no in-cluster manifests", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "kube/delightd")); err == nil {
		t.Error("kube/delightd/ exists; delightd is the operator, not a furnish piece -- an in-cluster delightd is the rejected pod model")
	}
}

// TestAggregatorRendersValidKube runs the real kustomize build for the whole
// environment and strictly decodes the rendered output. It proves the top-level
// kustomization aggregates real workloads (the render is non-trivial and every
// object strictly decodes) and that no object named delightd is among them --
// the operator must never render into the fleet it operates. The render is
// in-process via kustomize's own library, the same path furnish's buildPiece
// uses, so it needs no kubectl or kustomize binary on PATH.
func TestAggregatorRendersValidKube(t *testing.T) {
	root := repoRoot(t)

	m, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), filepath.Join(root, "kube/"))
	if err != nil {
		t.Fatalf("kustomize build kube/ failed: %v", err)
	}
	out, err := m.AsYaml()
	if err != nil {
		t.Fatalf("render kube/ to yaml: %v", err)
	}

	var workloads int
	objs, _ := decodeKube(t, out)
	for _, obj := range objs {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			workloads++
			if o.Name == "delightd" {
				t.Errorf("aggregator rendered a delightd Deployment (ns %q); the operator must not be a piece", o.Namespace)
			}
		case *appsv1.StatefulSet:
			workloads++
			if o.Name == "delightd" {
				t.Errorf("aggregator rendered a delightd StatefulSet (ns %q); the operator must not be a piece", o.Namespace)
			}
		case *corev1.Service:
			if o.Name == "delightd" {
				t.Errorf("aggregator rendered a delightd Service (ns %q); the operator must not be a piece", o.Namespace)
			}
		}
	}
	if workloads == 0 {
		t.Error("aggregator rendered no Deployment/StatefulSet at all; the whole-environment build is empty of workloads")
	}
}

// kustomizeResources decodes the resources list of one kustomization.yaml.
func kustomizeResources(t *testing.T, path string) []string {
	t.Helper()
	b := readFile(t, path)
	var k struct {
		Resources []string `json:"resources"`
	}
	if err := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), len(b)).Decode(&k); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return k.Resources
}

// aggregatorResources returns the piece names the top-level
// kube/kustomization.yaml lists, failing if it declares none.
func aggregatorResources(t *testing.T, root string) []string {
	t.Helper()
	res := kustomizeResources(t, filepath.Join(root, "kube/kustomization.yaml"))
	if len(res) == 0 {
		t.Fatal("aggregator declares no resources")
	}
	return res
}

// TestAggregatorEntriesArePieceDirs asserts every entry in the top-level
// aggregator resources list is a piece DIRECTORY carrying its own
// kustomization.yaml -- never a bare manifest file. furnish
// (cmd/delightd/furnish.go pieces()) reads this same list and runs
// `kubectl -k kube/<entry>` per entry, which requires a kustomization directory;
// a bare file entry silently breaks `furnish health/up/down` while
// `kubectl kustomize kube/` still succeeds. This is the regression guard for that
// gap (kubectl-kustomize alone does not catch it).
func TestAggregatorEntriesArePieceDirs(t *testing.T) {
	root := repoRoot(t)
	for _, r := range aggregatorResources(t, root) {
		info, err := os.Stat(filepath.Join(root, "kube", r))
		if err != nil {
			t.Errorf("aggregator entry %q: %v", r, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("aggregator entry %q is a file, not a piece directory; furnish feeds it to `kubectl -k` and will fail", r)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, "kube", r, "kustomization.yaml")); err != nil {
			t.Errorf("piece %q has no kustomization.yaml: %v", r, err)
		}
	}
}

// TestPieceManifestsDecode strict-decodes every manifest that each piece's own
// kustomization.yaml lists in its resources, directly and WITHOUT kubectl. It
// closes the gap where TestAggregatorRendersValidKube skips on a kubectl-absent
// runner. Driving off each piece's resources list (not a directory scan) means a
// dangling or mistyped manifest reference is caught here -- readFile fails on the
// missing file -- and a `.yml` reference is covered too, since we decode whatever
// the kustomization actually loads. decodeKube hard-fails on any bad object;
// realDocs == 0 catches a manifest that declares no resource documents.
func TestPieceManifestsDecode(t *testing.T) {
	root := repoRoot(t)
	for _, piece := range aggregatorResources(t, root) {
		pieceKust := filepath.Join(root, "kube", piece, "kustomization.yaml")
		for _, ref := range kustomizeResources(t, pieceKust) {
			rel := filepath.Join("kube", piece, ref)
			_, realDocs := decodeKube(t, readFile(t, filepath.Join(root, rel)))
			if realDocs == 0 {
				t.Errorf("%s declares no resource documents", rel)
			}
		}
	}
}
