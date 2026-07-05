package enablement

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "enablement.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAbsentReadsNotFound(t *testing.T) {
	s := openTemp(t)
	_, found, err := s.Get("ghost")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Error("absent project reported found=true")
	}
}

func TestDisableRequiresReason(t *testing.T) {
	s := openTemp(t)
	err := s.Put(Record{Project: "a", State: StateDisabled, Actor: "op"})
	if err == nil {
		t.Error("disable without reason accepted; the audit answer is mandatory")
	}
}

func TestUnknownStateRefused(t *testing.T) {
	s := openTemp(t)
	if err := s.Put(Record{Project: "a", State: "quiesced", Actor: "op"}); err == nil {
		t.Error("state outside the two-value surface accepted")
	}
}

func TestPutIsIdempotentAndStamps(t *testing.T) {
	s := openTemp(t)
	rec := Record{Project: "a", State: StateEnabled, Actor: "op"}
	if err := s.Put(rec); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := s.Put(rec); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	got, found, err := s.Get("a")
	if err != nil || !found {
		t.Fatalf("get after put: found=%v err=%v", found, err)
	}
	if got.State != StateEnabled || got.Actor != "op" {
		t.Errorf("roundtrip = %+v", got)
	}
	if got.ChangedAt.IsZero() {
		t.Error("ChangedAt not stamped on a bare put")
	}
}

func TestDisableRoundtrip(t *testing.T) {
	s := openTemp(t)
	if err := s.Put(Record{Project: "a", State: StateDisabled, Reason: "flaky disk", Actor: "op"}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _, _ := s.Get("a")
	if got.State != StateDisabled || got.Reason != "flaky disk" {
		t.Errorf("disable roundtrip = %+v", got)
	}
}
