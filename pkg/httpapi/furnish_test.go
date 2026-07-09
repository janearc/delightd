package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"delightd/config"
	"delightd/pkg/kube"
)

var fDeployGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
var fDeployGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}

// furnishFixture lays down a kube/ aggregator declaring one piece "delightd", whose dir
// holds a Deployment manifest -- the manifest root the handlers read.
func furnishFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "kustomization.yaml"), []byte("resources:\n  - delightd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pieceDir := filepath.Join(root, "delightd")
	if err := os.MkdirAll(pieceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dep := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: delightd\n  namespace: fleet\nspec:\n  replicas: 1\n"
	if err := os.WriteFile(filepath.Join(pieceDir, "deployment.yaml"), []byte(dep), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pieceDir, "kustomization.yaml"), []byte("resources:\n  - deployment.yaml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func furnishFakeClient(objs ...runtime.Object) *kube.Client {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	m.Add(fDeployGVK, meta.RESTScopeNamespace)
	return &kube.Client{
		Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objs...),
		Mapper:  m,
	}
}

func fLiveDeployment(ready int32) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(fDeployGVK)
	u.SetName("delightd")
	u.SetNamespace("fleet")
	_ = unstructured.SetNestedField(u.Object, int64(1), "spec", "replicas")
	_ = unstructured.SetNestedField(u.Object, int64(ready), "status", "readyReplicas")
	return u
}

// furnishServer builds a Server wired only with the furnish seams. A nil client leaves the
// provider unset, exercising the fail-loud path.
func furnishServer(kubeDir string, c *kube.Client) *Server {
	s := &Server{furnishKubeDir: kubeDir}
	if c != nil {
		s.furnishClient = func() (*kube.Client, error) { return c, nil }
	}
	return s
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body: %v (%s)", err, rr.Body.String())
	}
	return m
}

func TestFurnishPieces(t *testing.T) {
	s := furnishServer(furnishFixture(t), nil) // pieces needs no cluster
	rr := httptest.NewRecorder()
	s.handleFurnishPieces(rr, httptest.NewRequest(http.MethodGet, "/furnish/pieces", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	body := decodeBody(t, rr)
	pieces, _ := body["pieces"].([]any)
	if len(pieces) != 1 || pieces[0] != "delightd" {
		t.Errorf("pieces = %v, want [delightd]", body["pieces"])
	}
}

func TestFurnishHealth_NoClientIs503(t *testing.T) {
	s := furnishServer(furnishFixture(t), nil) // no cluster wired
	rr := httptest.NewRecorder()
	s.handleFurnishHealth(rr, httptest.NewRequest(http.MethodGet, "/furnish/health", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no cluster client should fail loud 503, got %d", rr.Code)
	}
}

func TestFurnishUp_UnknownPieceIs404BeforeCluster(t *testing.T) {
	s := furnishServer(furnishFixture(t), furnishFakeClient())
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/furnish/ghost/up", nil)
	req.SetPathValue("piece", "ghost")
	s.handleFurnishUp(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown piece should be 404, got %d", rr.Code)
	}
}

func TestFurnishUp_Applies(t *testing.T) {
	c := furnishFakeClient()
	fdc := c.Dynamic.(*dynamicfake.FakeDynamicClient)
	var applied []string
	fdc.PrependReactor("patch", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
		pa := a.(k8stesting.PatchActionImpl)
		if pa.GetPatchType() != types.ApplyPatchType {
			t.Errorf("patch type = %v, want server-side apply", pa.GetPatchType())
		}
		applied = append(applied, pa.Name)
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(fDeployGVK)
		u.SetName(pa.Name)
		u.SetNamespace(pa.Namespace)
		return true, u, nil
	})
	s := furnishServer(furnishFixture(t), c)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/furnish/delightd/up", nil)
	req.SetPathValue("piece", "delightd")
	s.handleFurnishUp(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["applied"] != true {
		t.Errorf("applied = %v, want true", body["applied"])
	}
	if len(applied) != 1 || applied[0] != "delightd" {
		t.Errorf("applied = %v, want [delightd]", applied)
	}
}

func TestFurnishHealth_GreenAndRed(t *testing.T) {
	fixture := furnishFixture(t)

	// A ready Deployment -> 200 healthy.
	s := furnishServer(fixture, furnishFakeClient(fLiveDeployment(1)))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/furnish/health/delightd", nil)
	req.SetPathValue("piece", "delightd")
	s.handleFurnishHealth(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("ready piece: code = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["healthy"] != true {
		t.Errorf("healthy = %v, want true", body["healthy"])
	}

	// Nothing seeded -> the declared Deployment is absent -> 503 unhealthy.
	s = furnishServer(fixture, furnishFakeClient())
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/furnish/health/delightd", nil)
	req.SetPathValue("piece", "delightd")
	s.handleFurnishHealth(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("absent object: code = %d, want 503 (%s)", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["healthy"] != false {
		t.Errorf("healthy = %v, want false", body["healthy"])
	}
}

// TestFurnishMutate_DeadlineIsLoud: a cluster op that runs out its deadline must answer
// 504 with the operation, the piece, and the bound that fired named in the body -- a
// wedged apiserver reads as "timed out after X", never a hang or an anonymous 500.
func TestFurnishMutate_DeadlineIsLoud(t *testing.T) {
	stuck := func(verb string) *kube.Client {
		c := furnishFakeClient()
		c.Dynamic.(*dynamicfake.FakeDynamicClient).PrependReactor(verb, "deployments",
			func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, context.DeadlineExceeded // the apiserver call ran out its deadline
			})
		return c
	}
	for _, tc := range []struct {
		op, verb, path string
		handle         func(*Server, http.ResponseWriter, *http.Request)
	}{
		{"furnish.up", "patch", "/furnish/delightd/up", (*Server).handleFurnishUp},
		{"furnish.down", "delete", "/furnish/delightd/down", (*Server).handleFurnishDown},
	} {
		s := furnishServer(furnishFixture(t), stuck(tc.verb))
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.path, nil)
		req.SetPathValue("piece", "delightd")
		tc.handle(s, rr, req)
		if rr.Code != http.StatusGatewayTimeout {
			t.Errorf("%s deadline: code = %d, want 504 (%s)", tc.op, rr.Code, rr.Body.String())
		}
		msg, _ := decodeBody(t, rr)["error"].(string)
		for _, want := range []string{tc.op, "delightd", "1m0s"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s deadline body should name %q (fail loud): %q", tc.op, want, msg)
			}
		}
	}
}

// TestUseFurnish_WiresReadyzToSharedClient: GET /readyz's apiserver check must go through
// the SAME lazily-built *kube.Client UseFurnish wires for the /furnish routes, rather than
// standing up its own kubeconfig+discovery+TLS on every call (M-minor, sprints#58).
func TestUseFurnish_WiresReadyzToSharedClient(t *testing.T) {
	var hits int
	apiserver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte("ok"))
	}))
	defer apiserver.Close()

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: t\n  cluster:\n    server: " +
		apiserver.URL + "\ncontexts:\n- name: t\n  context: {cluster: t, user: t}\ncurrent-context: t\nusers:\n- name: t\n  user:\n    token: abc\n"
	if err := os.WriteFile(kubeconfigPath, []byte(kubeconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := kube.FromKubeconfig(kubeconfigPath)
	if err != nil {
		t.Fatalf("FromKubeconfig: %v", err)
	}

	var provideCalls int
	s := &Server{cfg: &config.DelightConfig{}}
	s.cfg.System.MonitorRoot = t.TempDir()
	s.cfg.System.DaemonRoot = t.TempDir()
	s.cfg.System.ConfigRoot = t.TempDir()
	s.UseFurnish(furnishFixture(t), func() (*kube.Client, error) {
		provideCalls++
		return c, nil
	})

	rr := httptest.NewRecorder()
	s.handleReadyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz code = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if hits == 0 {
		t.Error("readyz never reached the apiserver through the shared client")
	}
	if provideCalls == 0 {
		t.Error("readyz did not go through the client provider UseFurnish wired")
	}
}

func TestFurnishDown_Removes(t *testing.T) {
	c := furnishFakeClient(fLiveDeployment(1))
	s := furnishServer(furnishFixture(t), c)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/furnish/delightd/down", nil)
	req.SetPathValue("piece", "delightd")
	s.handleFurnishDown(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (%s)", rr.Code, rr.Body.String())
	}
	if body := decodeBody(t, rr); body["removed"] != true {
		t.Errorf("removed = %v, want true", body["removed"])
	}
	if _, err := c.Dynamic.Resource(fDeployGVR).Namespace("fleet").Get(context.Background(), "delightd", metav1.GetOptions{}); err == nil {
		t.Error("object still present after furnish down")
	}
}
