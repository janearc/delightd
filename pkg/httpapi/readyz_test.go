package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"delightd/config"
)

// readyzServer builds a minimal Server exercising only what handleReadyz reads:
// the config roots and the injectable cluster probe.
func readyzServer(monitor, daemon, cfgRoot string, clusterErr error) *Server {
	c := &config.DelightConfig{}
	c.System.MonitorRoot = monitor
	c.System.DaemonRoot = daemon
	c.System.ConfigRoot = cfgRoot
	return &Server{
		cfg:          c,
		clusterReady: func(context.Context) error { return clusterErr },
	}
}

func doReadyz(t *testing.T, s *Server) (int, readinessResponse) {
	t.Helper()
	rr := httptest.NewRecorder()
	s.handleReadyz(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	var body readinessResponse
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatalf("decode /readyz body: %v", err)
	}
	return rr.Code, body
}

func findCheck(t *testing.T, body readinessResponse, name string) readinessCheck {
	t.Helper()
	for _, c := range body.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("readiness body missing check %q: %+v", name, body.Checks)
	return readinessCheck{}
}

func TestReadyzGreenWhenRootsAndClusterOK(t *testing.T) {
	dir := t.TempDir()
	s := readyzServer(dir, dir, dir, nil)
	code, body := doReadyz(t, s)
	if code != http.StatusOK || !body.Ready {
		t.Fatalf("ready roots+cluster: code=%d ready=%v, want 200/true; body=%+v", code, body.Ready, body)
	}
	if c := findCheck(t, body, "roots_readable"); !c.OK {
		t.Errorf("roots_readable should be ok: %+v", c)
	}
	if c := findCheck(t, body, "apiserver_reachable"); !c.OK {
		t.Errorf("apiserver_reachable should be ok: %+v", c)
	}
}

func TestReadyz503WhenRootMissing(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-mounted") // a mount that failed to attach
	s := readyzServer(missing, dir, dir, nil)
	code, body := doReadyz(t, s)
	if code != http.StatusServiceUnavailable || body.Ready {
		t.Fatalf("missing root: code=%d ready=%v, want 503/false", code, body.Ready)
	}
	c := findCheck(t, body, "roots_readable")
	if c.OK || !strings.Contains(c.Error, "monitor_root") {
		t.Errorf("roots_readable should be red and name monitor_root: %+v", c)
	}
}

func TestReadyz503WhenClusterUnreachable(t *testing.T) {
	dir := t.TempDir()
	s := readyzServer(dir, dir, dir, errors.New("apiserver unreachable"))
	code, body := doReadyz(t, s)
	if code != http.StatusServiceUnavailable || body.Ready {
		t.Fatalf("cluster down: code=%d ready=%v, want 503/false", code, body.Ready)
	}
	// roots still fine; only the cluster check is red.
	if c := findCheck(t, body, "roots_readable"); !c.OK {
		t.Errorf("roots_readable should stay ok when only the cluster is down: %+v", c)
	}
	c := findCheck(t, body, "apiserver_reachable")
	if c.OK || !strings.Contains(c.Error, "apiserver unreachable") {
		t.Errorf("apiserver_reachable should be red with the probe error: %+v", c)
	}
}

func TestReadyz503WhenRootUnset(t *testing.T) {
	dir := t.TempDir()
	s := readyzServer(dir, "", dir, nil) // daemon_root unset
	code, body := doReadyz(t, s)
	if code != http.StatusServiceUnavailable || body.Ready {
		t.Fatalf("unset root: code=%d ready=%v, want 503/false", code, body.Ready)
	}
	c := findCheck(t, body, "roots_readable")
	if c.OK || !strings.Contains(c.Error, "daemon_root is unset") {
		t.Errorf("roots_readable should name the unset daemon_root: %+v", c)
	}
}
