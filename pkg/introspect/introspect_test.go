package introspect

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"delightd/pkg/state"
)

// fakeFragments is a controllable FragmentChecker for tests.
type fakeFragments struct{ has map[string]bool }

func (f fakeFragments) HasBashFragment(service string) bool { return f.has[service] }

// backingUpMachine returns a machine driven into the backing_up state.
func backingUpMachine(t *testing.T, name string) *state.Machine {
	t.Helper()
	m := state.NewMachine(name)
	if err := m.Transition(context.Background(), state.EventTriggerBackup); err != nil {
		t.Fatalf("transition to backing_up: %v", err)
	}
	if got := m.GetState(); got != state.StateBackingUp {
		t.Fatalf("setup: state = %s, want backing_up", got)
	}
	return m
}

func TestServiceStatus_UnknownService(t *testing.T) {
	got := ServiceStatus("ghost", map[string]*state.Machine{}, fakeFragments{})

	if got.ServiceName != "ghost" {
		t.Errorf("service_name = %q, want ghost", got.ServiceName)
	}
	if got.IsKnownToDaemon {
		t.Error("unknown service must report is_known_to_daemon=false")
	}
	if got.IsActivelyBackingUp {
		t.Error("unknown service cannot be backing up")
	}
}

func TestServiceStatus_KnownFallow(t *testing.T) {
	machines := map[string]*state.Machine{"odysseus": state.NewMachine("odysseus")}

	got := ServiceStatus("odysseus", machines, fakeFragments{})

	if !got.IsKnownToDaemon {
		t.Error("configured service must report is_known_to_daemon=true")
	}
	if got.IsActivelyBackingUp {
		t.Error("fallow service must report is_actively_backing_up=false")
	}
}

func TestServiceStatus_KnownBackingUp(t *testing.T) {
	machines := map[string]*state.Machine{"paling": backingUpMachine(t, "paling")}

	got := ServiceStatus("paling", machines, fakeFragments{})

	if !got.IsActivelyBackingUp {
		t.Error("backing_up service must report is_actively_backing_up=true")
	}
}

func TestServiceStatus_FragmentReflectedRegardlessOfState(t *testing.T) {
	machines := map[string]*state.Machine{"comfyui": state.NewMachine("comfyui")}
	fc := fakeFragments{has: map[string]bool{"comfyui": true}}

	got := ServiceStatus("comfyui", machines, fc)

	if !got.HasBashFragment {
		t.Error("expected has_bash_fragment=true when the engine manages a shim")
	}
}

func TestHandler_UnknownReturns200(t *testing.T) {
	h := Handler(map[string]*state.Machine{}, fakeFragments{})

	req := httptest.NewRequest(http.MethodGet, "/projects/ghost/introspect", nil)
	req.SetPathValue("name", "ghost")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}

	var got ServiceBackupStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got.ServiceName != "ghost" || got.IsKnownToDaemon {
		t.Errorf("body = %+v, want unknown ghost", got)
	}
}

func TestHandler_KnownBackingUpWithFragment(t *testing.T) {
	machines := map[string]*state.Machine{"paling": backingUpMachine(t, "paling")}
	fc := fakeFragments{has: map[string]bool{"paling": true}}
	h := Handler(machines, fc)

	req := httptest.NewRequest(http.MethodGet, "/projects/paling/introspect", nil)
	req.SetPathValue("name", "paling")
	rr := httptest.NewRecorder()
	h(rr, req)

	var got ServiceBackupStatus
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !got.IsKnownToDaemon || !got.IsActivelyBackingUp || !got.HasBashFragment {
		t.Errorf("body = %+v, want known/backing_up/fragment all true", got)
	}
}
