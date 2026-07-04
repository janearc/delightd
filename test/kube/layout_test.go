// Package kube locks the kube/ manifest layout: one directory per piece of
// furniture, aggregated by a top-level kustomization. It is coverage over the
// restructure from flat kube/{deployment,service,kustomization}.yaml to
// kube/<piece>/..., so a later change cannot silently move a manifest back,
// drop it from the aggregator, or let the served port drift from the canonical
// control port. It reads the YAML off disk and parses it -- no cluster, no
// kubectl -- so it runs anywhere `go test` does.
package kube

import (
	"os"
	"path/filepath"
	"testing"

	"delightd/config"

	"gopkg.in/yaml.v3"
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

func readYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// TestLayoutFilesPresent asserts delightd's manifests live under kube/delightd/
// and no longer sit at the flat kube/ root.
func TestLayoutFilesPresent(t *testing.T) {
	root := repoRoot(t)

	present := []string{
		"kube/kustomization.yaml", // the aggregator
		"kube/delightd/deployment.yaml",
		"kube/delightd/service.yaml",
		"kube/delightd/kustomization.yaml",
	}
	for _, rel := range present {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("expected %s to exist: %v", rel, err)
		}
	}

	// The pre-restructure flat paths must be gone -- a partial revert that left
	// one behind would build a duplicate delightd under the aggregator.
	gone := []string{
		"kube/deployment.yaml",
		"kube/service.yaml",
	}
	for _, rel := range gone {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("stale flat manifest %s still present; it moved to kube/delightd/", rel)
		}
	}
}

type kustomization struct {
	Namespace string   `yaml:"namespace"`
	Resources []string `yaml:"resources"`
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestAggregatorWiresDelightd asserts the top-level kustomization pulls in the
// delightd piece, and delightd's own kustomization still names both manifests
// in the fleet namespace.
func TestAggregatorWiresDelightd(t *testing.T) {
	root := repoRoot(t)

	var agg kustomization
	readYAML(t, filepath.Join(root, "kube/kustomization.yaml"), &agg)
	if !contains(agg.Resources, "delightd") {
		t.Errorf("kube/kustomization.yaml resources = %v, want it to include \"delightd\"", agg.Resources)
	}

	var d kustomization
	readYAML(t, filepath.Join(root, "kube/delightd/kustomization.yaml"), &d)
	if d.Namespace != "fleet" {
		t.Errorf("kube/delightd/kustomization.yaml namespace = %q, want \"fleet\"", d.Namespace)
	}
	for _, want := range []string{"deployment.yaml", "service.yaml"} {
		if !contains(d.Resources, want) {
			t.Errorf("kube/delightd/kustomization.yaml resources = %v, want it to include %q", d.Resources, want)
		}
	}
}

type deployment struct {
	Metadata struct {
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Image string `yaml:"image"`
					Ports []struct {
						Name          string `yaml:"name"`
						ContainerPort int    `yaml:"containerPort"`
					} `yaml:"ports"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

type service struct {
	Metadata struct {
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Ports []struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"ports"`
	} `yaml:"spec"`
}

// TestDelightdManifestInvariants keeps the moved manifests tethered to the
// canonical control port and the fleet namespace -- the same port config locks
// in DefaultControlPort, so the wire the manifest serves and the port the code
// defaults to cannot drift apart.
func TestDelightdManifestInvariants(t *testing.T) {
	root := repoRoot(t)

	var d deployment
	readYAML(t, filepath.Join(root, "kube/delightd/deployment.yaml"), &d)
	if d.Metadata.Namespace != "fleet" {
		t.Errorf("deployment namespace = %q, want \"fleet\"", d.Metadata.Namespace)
	}
	if len(d.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("deployment has no containers")
	}
	c := d.Spec.Template.Spec.Containers[0]
	if c.Image == "" {
		t.Error("deployment container image is empty")
	}
	var control int
	for _, p := range c.Ports {
		if p.Name == "control" {
			control = p.ContainerPort
		}
	}
	if control != config.DefaultControlPort {
		t.Errorf("deployment control containerPort = %d, want DefaultControlPort %d", control, config.DefaultControlPort)
	}

	var s service
	readYAML(t, filepath.Join(root, "kube/delightd/service.yaml"), &s)
	if s.Metadata.Namespace != "fleet" {
		t.Errorf("service namespace = %q, want \"fleet\"", s.Metadata.Namespace)
	}
	var svcPort int
	for _, p := range s.Spec.Ports {
		if p.Name == "control" {
			svcPort = p.Port
		}
	}
	if svcPort != config.DefaultControlPort {
		t.Errorf("service control port = %d, want DefaultControlPort %d", svcPort, config.DefaultControlPort)
	}
}
