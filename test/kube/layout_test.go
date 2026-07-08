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

	"delightd/config"

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

	objs, _ := decodeKube(t, readFile(t, filepath.Join(root, "kube/delightd/deployment.yaml")))
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

	objs, _ = decodeKube(t, readFile(t, filepath.Join(root, "kube/delightd/service.yaml")))
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
// kube -- not just that the files parse. The render is in-process via kustomize's
// own library, the same path furnish's buildPiece uses, so it needs no kubectl or
// kustomize binary on PATH.
func TestAggregatorRendersValidKube(t *testing.T) {
	root := repoRoot(t)

	for _, target := range []string{"kube/", "kube/delightd/"} {
		m, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), filepath.Join(root, target))
		if err != nil {
			t.Fatalf("kustomize build %s failed: %v", target, err)
		}
		out, err := m.AsYaml()
		if err != nil {
			t.Fatalf("render %s to yaml: %v", target, err)
		}

		var haveDeploy, haveSvc bool
		objs, _ := decodeKube(t, out)
		for _, obj := range objs {
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
