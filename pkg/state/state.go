package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"sync"
	"time"
)

type State string

const (
	StateFallow     State = "fallow"
	StateMonitoring State = "monitoring"
	StateBackingUp  State = "backing_up"
	StateError      State = "error"
)

type Event string

const (
	EventChurnDetected Event = "churn_detected"
	EventTriggerBackup Event = "trigger_backup"
	EventBackupSuccess Event = "backup_success"
	EventBackupFail    Event = "backup_fail"
	EventClearError    Event = "clear_error"
)

// Machine is a thread-safe state machine governing the backup loop
type Machine struct {
	mu           sync.RWMutex
	ProjectName  string
	CurrentState State
	ErrorCount   int
	LastActivity time.Time
	NextRetryAt  time.Time
}

func NewMachine(projectName string) *Machine {
	return &Machine{
		ProjectName:  projectName,
		CurrentState: StateFallow,
		LastActivity: time.Now(),
	}
}

// Transition handles the core state progression with strict locking.
func (m *Machine) Transition(ctx context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	prevState := m.CurrentState

	switch e {
	case EventChurnDetected:
		if m.CurrentState == StateFallow {
			m.CurrentState = StateMonitoring
		}
	case EventTriggerBackup:
		// Can trigger from fallow (manual poke), monitoring (auto), or error (retry)
		if m.CurrentState == StateFallow || m.CurrentState == StateMonitoring || m.CurrentState == StateError {
			// Thundering Herd / Backoff Protection
			if m.CurrentState == StateError && time.Now().Before(m.NextRetryAt) {
				return fmt.Errorf("in backoff period until %v", m.NextRetryAt)
			}
			m.CurrentState = StateBackingUp
		}
	case EventBackupSuccess:
		if m.CurrentState == StateBackingUp {
			m.CurrentState = StateFallow
			m.ErrorCount = 0
			m.NextRetryAt = time.Time{}
		}
	case EventBackupFail:
		if m.CurrentState == StateBackingUp {
			m.CurrentState = StateError
			m.ErrorCount++

			// Exponential backoff with random Jitter to prevent Thundering Herds on retry
			baseWait := time.Minute * time.Duration(math.Pow(2, float64(m.ErrorCount)))
			if baseWait > time.Hour {
				baseWait = time.Hour
			}
			jitter := time.Duration(rand.Intn(60)) * time.Second
			m.NextRetryAt = time.Now().Add(baseWait).Add(jitter)

			slog.Warn("backup failed, entering de-escalating backoff",
				"project", m.ProjectName,
				"error_count", m.ErrorCount,
				"next_retry", m.NextRetryAt)
		}
	case EventClearError:
		// Allows the control port to manually reset a stuck machine
		if m.CurrentState == StateError {
			m.CurrentState = StateFallow
			m.ErrorCount = 0
			m.NextRetryAt = time.Time{}
		}
	default:
		return fmt.Errorf("unknown event: %s", e)
	}

	if prevState != m.CurrentState {
		m.LastActivity = time.Now()
		slog.Info("state transition",
			"project", m.ProjectName,
			"event", e,
			"from", prevState,
			"to", m.CurrentState)
	}

	return nil
}

// GetState returns a thread-safe snapshot of the current state.
func (m *Machine) GetState() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.CurrentState
}

// GetDiagnostics returns state information for the control port
func (m *Machine) GetDiagnostics() map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return map[string]interface{}{
		"state":         m.CurrentState,
		"error_count":   m.ErrorCount,
		"last_activity": m.LastActivity,
		"next_retry":    m.NextRetryAt,
	}
}

// CanRetry safely checks if the backoff period has expired
func (m *Machine) CanRetry() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.CurrentState != StateError {
		return true
	}
	return time.Now().After(m.NextRetryAt)
}

// WatchesForChurn reports whether the machine is in a state that should poll the
// git oracle for changes. Only fallow and monitoring watch; while backing up or
// in error backoff there is nothing to react to, so polling is skipped.
func (m *Machine) WatchesForChurn() bool {
	switch m.GetState() {
	case StateFallow, StateMonitoring:
		return true
	default:
		return false
	}
}

// AdvanceOnChurn transitions the machine in response to detected git churn,
// selecting the correct event for the current state. It encapsulates the
// fallow->monitoring->backing_up progression so callers do not branch on state
// themselves. It is a no-op for states that do not react to churn.
func (m *Machine) AdvanceOnChurn(ctx context.Context) error {
	switch m.GetState() {
	case StateFallow:
		// fallow + churn: the project was quiet and just changed. We do not back
		// up on the first sighting; we move to monitoring so the next churn check
		// confirms sustained activity before we spend a checkpoint.
		return m.Transition(ctx, EventChurnDetected)
	case StateMonitoring:
		// monitoring + churn: we already saw activity last tick and it is still
		// changing, so this is the signal to actually trigger the backup.
		return m.Transition(ctx, EventTriggerBackup)
	default:
		// backing_up / error: a checkpoint is already in flight or we are in
		// backoff. Fresh churn changes nothing until that resolves, so no-op.
		return nil
	}
}
