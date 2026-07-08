package kube

import (
	"os"
	"path/filepath"
	"testing"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://10.1.2.3:6443
    insecure-skip-tls-verify: true
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user:
    token: abc123
`

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRESTConfig_FromKubeconfigFixture(t *testing.T) {
	cfg, err := RESTConfig(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if cfg.Host != "https://10.1.2.3:6443" {
		t.Errorf("Host = %q, want the fixture server", cfg.Host)
	}
	if cfg.BearerToken != "abc123" {
		t.Errorf("BearerToken = %q, want it loaded from the fixture", cfg.BearerToken)
	}
}

func TestFromKubeconfig_BuildsClientWithoutNetwork(t *testing.T) {
	// FromKubeconfig builds the dynamic client and a deferred RESTMapper; the mapper is
	// lazy, so construction touches no cluster and must succeed on a valid kubeconfig.
	c, err := FromKubeconfig(writeKubeconfig(t))
	if err != nil {
		t.Fatalf("FromKubeconfig: %v", err)
	}
	if c.Config == nil || c.Dynamic == nil || c.Mapper == nil {
		t.Fatalf("client not fully constructed: %+v", c)
	}
}

func TestRESTConfig_MissingKubeconfig(t *testing.T) {
	// An explicit path that does not exist is an error, not a silent empty config.
	if _, err := RESTConfig(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("RESTConfig with a missing kubeconfig: want error, got nil")
	}
}
