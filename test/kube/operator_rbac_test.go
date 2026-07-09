package kube

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
)

// rbacScheme strict-decodes the operator bootstrap manifest: core kinds (Namespace,
// ServiceAccount, Secret) plus rbac kinds (ClusterRole, ClusterRoleBinding).
var rbacScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := rbacv1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}()

// TestOperatorRBAC strict-decodes deploy/delightd-operator-rbac.yaml -- the credential a
// human applies with admin at bootstrap -- so a typo fails here, not at the live apply.
// It also locks in the least-privilege guarantee: the ClusterRole grants no read on
// Secrets (those are injected out-of-band, never furnished).
func TestOperatorRBAC(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "delightd-operator-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, rbacScheme, rbacScheme,
		kjson.SerializerOptions{Yaml: true, Strict: true})

	kinds := map[string]bool{}
	var clusterRole *rbacv1.ClusterRole
	for i, doc := range splitDocs(t, data) {
		if isCommentOnly(doc) {
			if i == 0 {
				continue // the file header before the first ---
			}
			t.Fatalf("commented-out resource at document %d", i)
		}
		obj, gvk, err := dec.Decode(doc, nil, nil)
		if err != nil {
			t.Fatalf("decode document %d (gvk=%v): %v\n---\n%s", i, gvk, err, doc)
		}
		kinds[gvk.Kind] = true
		if cr, ok := obj.(*rbacv1.ClusterRole); ok {
			clusterRole = cr
		}
	}

	for _, want := range []string{"Namespace", "ServiceAccount", "Secret", "ClusterRole", "ClusterRoleBinding"} {
		if !kinds[want] {
			t.Errorf("bootstrap manifest missing a %s", want)
		}
	}

	if clusterRole == nil {
		t.Fatal("no ClusterRole decoded")
	}
	for _, rule := range clusterRole.Rules {
		for _, r := range rule.Resources {
			if r == "secrets" {
				t.Error("ClusterRole grants access to secrets -- furnish must never read secrets")
			}
		}
	}
}
