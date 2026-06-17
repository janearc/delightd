package state

import (
	"context"
	"testing"
)

func TestWatchesForChurn(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("p")

	if !m.WatchesForChurn() {
		t.Error("fallow machine should watch for churn")
	}

	if err := m.Transition(ctx, EventChurnDetected); err != nil {
		t.Fatal(err)
	}
	if !m.WatchesForChurn() {
		t.Error("monitoring machine should watch for churn")
	}

	if err := m.Transition(ctx, EventTriggerBackup); err != nil {
		t.Fatal(err)
	}
	if m.WatchesForChurn() {
		t.Error("backing_up machine must not watch for churn")
	}
}

func TestAdvanceOnChurn(t *testing.T) {
	ctx := context.Background()

	fallow := NewMachine("fallow")
	if err := fallow.AdvanceOnChurn(ctx); err != nil {
		t.Fatal(err)
	}
	if got := fallow.GetState(); got != StateMonitoring {
		t.Errorf("fallow + churn -> %s, want monitoring", got)
	}

	monitoring := NewMachine("monitoring")
	if err := monitoring.Transition(ctx, EventChurnDetected); err != nil {
		t.Fatal(err)
	}
	if err := monitoring.AdvanceOnChurn(ctx); err != nil {
		t.Fatal(err)
	}
	if got := monitoring.GetState(); got != StateBackingUp {
		t.Errorf("monitoring + churn -> %s, want backing_up", got)
	}

	// a machine already backing up does not react to churn
	backing := NewMachine("backing")
	if err := backing.Transition(ctx, EventTriggerBackup); err != nil {
		t.Fatal(err)
	}
	if err := backing.AdvanceOnChurn(ctx); err != nil {
		t.Fatal(err)
	}
	if got := backing.GetState(); got != StateBackingUp {
		t.Errorf("backing_up + churn -> %s, want unchanged backing_up", got)
	}
}
