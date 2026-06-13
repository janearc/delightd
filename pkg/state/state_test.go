package state

import (
	"context"
	"testing"
)

func TestMachineTransitions(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("test-proj")

	if m.GetState() != StateFallow {
		t.Errorf("expected state fallow, got %s", m.GetState())
	}

	// Fallow -> Monitoring
	err := m.Transition(ctx, EventChurnDetected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.GetState() != StateMonitoring {
		t.Errorf("expected monitoring, got %s", m.GetState())
	}

	// Monitoring -> BackingUp
	err = m.Transition(ctx, EventTriggerBackup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.GetState() != StateBackingUp {
		t.Errorf("expected backing_up, got %s", m.GetState())
	}

	// BackingUp -> Fallow
	err = m.Transition(ctx, EventBackupSuccess)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m.GetState() != StateFallow {
		t.Errorf("expected fallow, got %s", m.GetState())
	}
}

func TestMachineErrorAndBackoff(t *testing.T) {
	ctx := context.Background()
	m := NewMachine("test-proj")

	m.Transition(ctx, EventTriggerBackup)
	if m.GetState() != StateBackingUp {
		t.Fatalf("expected backing_up, got %s", m.GetState())
	}

	// Simulate failure
	m.Transition(ctx, EventBackupFail)
	if m.GetState() != StateError {
		t.Fatalf("expected error, got %s", m.GetState())
	}
	if m.ErrorCount != 1 {
		t.Errorf("expected error count 1, got %d", m.ErrorCount)
	}

	// Immediate retry should fail due to backoff
	err := m.Transition(ctx, EventTriggerBackup)
	if err == nil {
		t.Errorf("expected backoff error, got nil")
	}

	// Test CanRetry
	if m.CanRetry() {
		t.Errorf("CanRetry should be false during backoff")
	}

	// Clear error manually
	m.Transition(ctx, EventClearError)
	if m.GetState() != StateFallow {
		t.Fatalf("expected fallow after clear, got %s", m.GetState())
	}
	if m.ErrorCount != 0 {
		t.Errorf("expected error count to be cleared")
	}
}

func TestGetDiagnostics(t *testing.T) {
	m := NewMachine("test-proj")
	diag := m.GetDiagnostics()
	if diag["state"] != StateFallow {
		t.Errorf("expected state fallow in diagnostics")
	}
	if diag["error_count"] != 0 {
		t.Errorf("expected error count 0")
	}
}
