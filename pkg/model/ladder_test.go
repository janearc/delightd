package model

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestTierDeclared(t *testing.T) {
	present := DeploymentDescriptor{Name: "p", Location: writeTemp(t, "weights.bin", "x"), Backend: BackendTransformers}
	if r := present.tierDeclared(); r.State != StateGreen {
		t.Fatalf("present weights should be GREEN, got %v (%s)", r.State, r.Detail)
	}

	missing := DeploymentDescriptor{Name: "m", Location: "/no/such/path.gguf", Backend: BackendTransformers}
	if r := missing.tierDeclared(); r.State != StateRed {
		t.Fatalf("missing weights should be RED, got %v", r.State)
	}

	// an ollama tag is a handle, not a path -> GREEN here (registration is a loadable check).
	tag := DeploymentDescriptor{Name: "o", Location: "mistral:latest", Backend: BackendOllama}
	if r := tag.tierDeclared(); r.State != StateGreen {
		t.Fatalf("ollama tag should be GREEN at declared, got %v", r.State)
	}
}

func TestLadder_DeclaredAndReachableIsGreen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port := splitHostPort(t, srv.URL)

	d := DeploymentDescriptor{
		Name: "live", Location: writeTemp(t, "w.bin", "x"), Architecture: ArchEncodeDecode,
		Role: RoleCompletion, Backend: BackendTransformers,
		TraefikRoute: TraefikRoute{Host: host, ServicePort: port},
	}
	rep := d.Ladder()
	if rep.Overall != StateGreen {
		t.Fatalf("declared+reachable should be GREEN overall, got %v (%+v)", rep.Overall, rep.Tiers)
	}
	if len(rep.Tiers) != 2 {
		t.Fatalf("want 2 tiers, got %d", len(rep.Tiers))
	}
}

func TestLadder_UnreachableIsRed(t *testing.T) {
	d := DeploymentDescriptor{
		Name: "down", Location: writeTemp(t, "w.bin", "x"), Backend: BackendTransformers,
		TraefikRoute: TraefikRoute{Host: "127.0.0.1", ServicePort: 1},
	}
	if rep := d.Ladder(); rep.Overall != StateRed {
		t.Fatalf("unreachable endpoint should be RED overall, got %v", rep.Overall)
	}
}

func TestWorstState(t *testing.T) {
	if worstState([]TierResult{{State: StateGreen}, {State: StateRed}}) != StateRed {
		t.Fatal("RED should win")
	}
	if worstState([]TierResult{{State: StateGreen}, {State: StateGreen}}) != StateGreen {
		t.Fatal("all GREEN -> GREEN")
	}
}

func splitHostPort(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	return u.Hostname(), port
}
