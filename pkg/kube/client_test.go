package kube

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// An explicit path that does not exist is an error, not a silent empty config -- and
	// the error names the path it tried, so the failure points at the fix.
	path := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := RESTConfig(path)
	if err == nil {
		t.Fatal("RESTConfig with a missing kubeconfig: want error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the kubeconfig path it tried: %q", err)
	}
}

// writeKubeconfigForServer writes a kubeconfig whose cluster server is the given URL, so
// a probe can be pointed at an httptest apiserver stand-in.
func writeKubeconfigForServer(t *testing.T, server string) string {
	t.Helper()
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
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
`, server)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPIServerReady_GreenOn200(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintln(w, "ok")
	}))
	defer srv.Close()
	if err := APIServerReady(context.Background(), writeKubeconfigForServer(t, srv.URL)); err != nil {
		t.Fatalf("APIServerReady on a healthy apiserver: %v", err)
	}
	// It must hit the apiserver's own /readyz, the endpoint kubectl --raw fetched.
	if gotPath != "/readyz" {
		t.Errorf("probed path = %q, want /readyz", gotPath)
	}
}

func TestAPIServerReady_RedOn500(t *testing.T) {
	// A 500 (apiserver up but not ready) is a red check, not a pass.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if err := APIServerReady(context.Background(), writeKubeconfigForServer(t, srv.URL)); err == nil {
		t.Fatal("APIServerReady on a 500 apiserver: want error, got nil")
	}
}

func TestAPIServerReady_RedWhenKubeconfigMissing(t *testing.T) {
	// An explicit kubeconfig path that does not exist fails at config load, before any
	// network -- surfaced as an error, never a silent pass.
	if err := APIServerReady(context.Background(), filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("APIServerReady with a missing kubeconfig: want error, got nil")
	}
}

func TestFromKubeconfig_MissingPathErrors(t *testing.T) {
	// FromKubeconfig surfaces a bad explicit path rather than building a client against
	// an empty config.
	if _, err := FromKubeconfig(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("FromKubeconfig with a missing kubeconfig: want error, got nil")
	}
}

func TestAPIServerReady_RedWhenUnreachable(t *testing.T) {
	// Nothing listening -> the probe fails (fast, connection refused), never hangs green.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if err := APIServerReady(context.Background(), writeKubeconfigForServer(t, url)); err == nil {
		t.Fatal("APIServerReady against a closed server: want error, got nil")
	}
}
