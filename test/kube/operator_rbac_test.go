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
// ServiceAccount, Secret) plus rbac kinds (ClusterRole, ClusterRoleBinding, Role,
// RoleBinding).
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

// rbacAllowedNamespaces is where a namespaced Role/RoleBinding may legitimately grant
// workload writes -- exactly the namespaces the meubilair pieces declare
// (kube/*/kustomization.yaml). A new namespace showing up here is a reviewed addition to
// this test, not a silent pass; see the shape comment in
// deploy/delightd-operator-rbac.yaml for how this was verified.
var rbacAllowedNamespaces = map[string]bool{"fleet": true}

// TestOperatorRBAC strict-decodes deploy/delightd-operator-rbac.yaml -- the credential a
// human applies with admin at bootstrap -- so a typo fails here, not at the live apply.
//
// It also locks in the least-privilege SHAPE (sprints#58 M8): the ClusterRole must carry
// no resource grant at all (only the non-resource /readyz-style endpoints, which have no
// namespace concept), every workload/config write must arrive via a namespaced Role bound
// to an allowlisted namespace, and neither a wildcard resource/verb/apiGroup nor a grant on
// secrets may appear anywhere. A ClusterRole with `resources: ["*"]` (or the grants simply
// left in a ClusterRole) fails this test even though it would have passed the old
// secrets-only check.
func TestOperatorRBAC(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "delightd-operator-rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := kjson.NewSerializerWithOptions(kjson.DefaultMetaFactory, rbacScheme, rbacScheme,
		kjson.SerializerOptions{Yaml: true, Strict: true})

	kinds := map[string]bool{}
	var clusterRole *rbacv1.ClusterRole
	var clusterRoleBindings []*rbacv1.ClusterRoleBinding
	var roles []*rbacv1.Role
	var roleBindings []*rbacv1.RoleBinding

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
		switch o := obj.(type) {
		case *rbacv1.ClusterRole:
			clusterRole = o
		case *rbacv1.ClusterRoleBinding:
			clusterRoleBindings = append(clusterRoleBindings, o)
		case *rbacv1.Role:
			roles = append(roles, o)
		case *rbacv1.RoleBinding:
			roleBindings = append(roleBindings, o)
		}
	}

	for _, want := range []string{"Namespace", "ServiceAccount", "Secret", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding"} {
		if !kinds[want] {
			t.Errorf("bootstrap manifest missing a %s", want)
		}
	}

	if clusterRole == nil {
		t.Fatal("no ClusterRole decoded")
	}

	// The whole point of the fix: the ClusterRole grants no resource at all, cluster-wide
	// or otherwise -- only non-resource URLs, which cannot be expressed any other way. Any
	// Resources on a ClusterRole rule is a cluster-wide grant and a direct regression of
	// M8 (the escalation path was create/update/patch/delete on workload kinds via a
	// ClusterRoleBinding).
	for _, rule := range clusterRole.Rules {
		if len(rule.Resources) > 0 {
			t.Errorf("ClusterRole %q grants resources %v cluster-wide -- workload/config grants belong in a namespaced Role, not here", clusterRole.Name, rule.Resources)
		}
	}

	assertNoWildcardsOrSecrets(t, "ClusterRole/"+clusterRole.Name, clusterRole.Rules)

	// Every ClusterRoleBinding must bind this same resource-free ClusterRole -- a second
	// cluster-scoped role slipping in under a different binding would defeat the check
	// above.
	for _, crb := range clusterRoleBindings {
		if crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != clusterRole.Name {
			t.Errorf("ClusterRoleBinding %s: roleRef = %s/%s, want ClusterRole/%s", crb.Name, crb.RoleRef.Kind, crb.RoleRef.Name, clusterRole.Name)
		}
	}

	if len(roles) == 0 {
		t.Fatal("no Role decoded -- workload/config writes must arrive via a namespaced Role")
	}
	gotCreate := map[string]bool{} // resource -> some Role grants it create, across all Roles
	for _, role := range roles {
		if !rbacAllowedNamespaces[role.Namespace] {
			t.Errorf("Role %s/%s: namespace not in the declared allowlist %v -- a new namespace is a reviewed test change", role.Namespace, role.Name, rbacAllowedNamespaces)
		}
		assertNoWildcardsOrSecrets(t, "Role/"+role.Namespace+"/"+role.Name, role.Rules)
		for _, rule := range role.Rules {
			for _, v := range rule.Verbs {
				if v != "create" {
					continue
				}
				for _, r := range rule.Resources {
					gotCreate[r] = true
				}
			}
		}
	}

	// Positive check: the fix must not have quietly dropped a kind furnish needs to
	// converge -- every workload kind the pre-fix ClusterRole granted create on must still
	// get create, just via the namespaced Role now.
	for _, want := range []string{"deployments", "statefulsets", "daemonsets", "replicasets", "jobs", "cronjobs"} {
		if !gotCreate[want] {
			t.Errorf("no namespaced Role grants create on %q -- furnish would fail to converge that kind", want)
		}
	}

	if len(roleBindings) == 0 {
		t.Fatal("no RoleBinding decoded")
	}
	for _, rb := range roleBindings {
		if !rbacAllowedNamespaces[rb.Namespace] {
			t.Errorf("RoleBinding %s/%s: namespace not in the declared allowlist %v", rb.Namespace, rb.Name, rbacAllowedNamespaces)
		}
		// A RoleBinding CAN legally reference a ClusterRole (scoping its rules to one
		// namespace) -- that is not what this manifest does, so assert the actual shape;
		// a switch to that pattern is a reviewed change to this test too.
		if rb.RoleRef.Kind != "Role" {
			t.Errorf("RoleBinding %s/%s: roleRef.Kind = %q, want Role", rb.Namespace, rb.Name, rb.RoleRef.Kind)
		}
	}
}

// assertNoWildcardsOrSecrets fails the test if any rule in rules grants a wildcard
// apiGroup/resource/verb or any access to secrets. where names the object+namespace for
// the failure message.
func assertNoWildcardsOrSecrets(t *testing.T, where string, rules []rbacv1.PolicyRule) {
	t.Helper()
	for _, rule := range rules {
		for _, g := range rule.APIGroups {
			if g == "*" {
				t.Errorf("%s: wildcard apiGroup", where)
			}
		}
		for _, r := range rule.Resources {
			if r == "*" {
				t.Errorf("%s: wildcard resource", where)
			}
			if r == "secrets" {
				t.Errorf("%s: grants access to secrets -- furnish must never read secrets", where)
			}
		}
		for _, v := range rule.Verbs {
			if v == "*" {
				t.Errorf("%s: wildcard verb", where)
			}
		}
	}
}
