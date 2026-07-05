package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"delightd/config"
	"delightd/pkg/enablement"
)

// fakeEnablement is an in-memory enablementStore for handler tests.
type fakeEnablement map[string]enablement.Record

func (f fakeEnablement) Get(p string) (enablement.Record, bool, error) {
	r, ok := f[p]
	return r, ok, nil
}
func (f fakeEnablement) Put(r enablement.Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	f[r.Project] = r
	return nil
}

func stateServer(t *testing.T, st enablementStore) *http.ServeMux {
	t.Helper()
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "alpha"}}}
	s := New(cfg, nil, fakeFragments{}, nil, true, nil)
	if st != nil {
		s.UseEnablement(st)
	}
	return s.Mux()
}

func do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rr
}

// The doctrine: no store means 503 degraded, never an invented answer.
func TestStateFailsClosedWithoutStore(t *testing.T) {
	mux := stateServer(t, nil)
	for _, req := range [][2]string{{"GET", "/state"}, {"GET", "/state/alpha"}, {"PUT", "/state/alpha"}} {
		if rr := do(t, mux, req[0], req[1], "{}"); rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without store: code = %d, want 503", req[0], req[1], rr.Code)
		}
	}
}

// The doctrine: an absent record reads disabled, and the project still appears.
func TestAbsentReadsDisabled(t *testing.T) {
	mux := stateServer(t, fakeEnablement{})
	rr := do(t, mux, "GET", "/state/alpha", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var got stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != enablement.StateDisabled || got.Recorded {
		t.Errorf("absent record rendered %+v, want disabled/unrecorded", got)
	}
}

func TestUnknownProjectRefused(t *testing.T) {
	mux := stateServer(t, fakeEnablement{})
	if rr := do(t, mux, "GET", "/state/ghost", ""); rr.Code != http.StatusNotFound {
		t.Errorf("GET unknown: code = %d, want 404", rr.Code)
	}
	if rr := do(t, mux, "PUT", "/state/ghost", `{"state":"enabled","actor":"op"}`); rr.Code != http.StatusNotFound {
		t.Errorf("PUT unknown: code = %d, want 404", rr.Code)
	}
}

func TestPutValidatesAndRoundtrips(t *testing.T) {
	store := fakeEnablement{}
	mux := stateServer(t, store)

	if rr := do(t, mux, "PUT", "/state/alpha", `{"state":"quiesced","actor":"op"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("unknown state: code = %d, want 400", rr.Code)
	}
	if rr := do(t, mux, "PUT", "/state/alpha", `{"state":"disabled","actor":"op"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("disable without reason: code = %d, want 400", rr.Code)
	}

	rr := do(t, mux, "PUT", "/state/alpha", `{"state":"disabled","reason":"flaky disk","actor":"op"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("valid disable: code = %d, body %s", rr.Code, rr.Body.String())
	}
	var got stateResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.State != enablement.StateDisabled || !got.Recorded || got.Reason != "flaky disk" {
		t.Errorf("disable rendered %+v", got)
	}

	var all struct {
		Projects []stateResponse `json:"projects"`
	}
	rrAll := do(t, mux, "GET", "/state", "")
	if err := json.Unmarshal(rrAll.Body.Bytes(), &all); err != nil {
		t.Fatalf("decode all: %v", err)
	}
	if len(all.Projects) != 1 || all.Projects[0].State != enablement.StateDisabled {
		t.Errorf("GET /state = %+v", all)
	}
}
