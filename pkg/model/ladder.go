package model

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	observabilityv1 "github.com/janearc/big-little-mesh/gen/go/observability/v1"
)

// The model-health ladder (docs/model-hosting.md §4): a model's "green" is not one bit but
// a set of rungs, cheap -> expensive, reported in the fleet's HealthState. The always-on
// tiers here -- declared and reachable -- are the continuous signal; loadable and integrity
// are summoned and land later.

type HealthTier string

const (
	TierDeclared  HealthTier = "declared"  // the manifest declares it and its weights are present
	TierReachable HealthTier = "reachable" // its endpoint answers
)

// Health is the fleet contract enum observability.v1.HealthState -- not a homegrown string.
// Its numeric order is its severity (UNSPECIFIED < GREEN < YELLOW < RED < EXHAUSTED), so a
// worst-wins roll-up is just the max; "not assessed" is HEALTH_STATE_UNSPECIFIED, a typed
// value rather than a null. JSON renders the enum's name.
type Health observabilityv1.HealthState

const (
	StateUnspecified = Health(observabilityv1.HealthState_HEALTH_STATE_UNSPECIFIED)
	StateGreen       = Health(observabilityv1.HealthState_HEALTH_STATE_GREEN)
	StateYellow      = Health(observabilityv1.HealthState_HEALTH_STATE_YELLOW)
	StateRed         = Health(observabilityv1.HealthState_HEALTH_STATE_RED)
)

func (h Health) MarshalJSON() ([]byte, error) {
	return json.Marshal(observabilityv1.HealthState(h).String())
}

// TierResult is one rung's verdict.
type TierResult struct {
	Tier   HealthTier `json:"tier"`
	State  Health     `json:"state"`
	Detail string     `json:"detail,omitempty"`
}

// LadderReport is the per-deployment roll-up: the assessed rungs and a worst-wins overall.
type LadderReport struct {
	Deployment string       `json:"deployment"`
	Backend    Backend      `json:"backend"`
	Overall    Health       `json:"overall"`
	Tiers      []TierResult `json:"tiers"`
}

// tierDeclared checks the weights are where the descriptor says. A path / HF-cache location
// is stat-ed; an ollama tag is a backend handle (not a path), so its registration check
// belongs to the later loadable tier -- here it is GREEN as a coherent declaration.
func (d DeploymentDescriptor) tierDeclared() TierResult {
	if d.IsHFCacheLocation() {
		if _, err := os.Stat(d.Location); err != nil {
			return TierResult{TierDeclared, StateRed, "weights not found at " + d.Location}
		}
		return TierResult{TierDeclared, StateGreen, "weights present on disk"}
	}
	return TierResult{TierDeclared, StateGreen, "declared (location is a backend handle, not a path)"}
}

// tierReachable probes the endpoint -- the always-on liveness rung.
func (d DeploymentDescriptor) tierReachable() TierResult {
	p := ProbeURL(d.HealthURL(), 2*time.Second)
	if !p.Reachable {
		return TierResult{TierReachable, StateRed, p.URL + " unreachable"}
	}
	return TierResult{TierReachable, StateGreen, fmt.Sprintf("%s -> %d", p.URL, p.Status)}
}

// Ladder assesses the always-on tiers and rolls them up worst-wins.
func (d DeploymentDescriptor) Ladder() LadderReport {
	tiers := []TierResult{d.tierDeclared(), d.tierReachable()}
	return LadderReport{
		Deployment: d.Name,
		Backend:    d.Backend,
		Overall:    worstState(tiers),
		Tiers:      tiers,
	}
}

// Ladders assesses one named deployment, or every deployment when name is empty. found is
// false only for a name that is not in the set.
func (s DeploymentSet) Ladders(name string) (reports []LadderReport, found bool) {
	if name != "" {
		d, ok := s.ByName(name)
		if !ok {
			return nil, false
		}
		return []LadderReport{d.Ladder()}, true
	}
	for _, d := range s.Deployments {
		reports = append(reports, d.Ladder())
	}
	return reports, true
}

// worstState returns the worst rung's state. HealthState's numeric order is its severity,
// so worst-wins is the max.
func worstState(tiers []TierResult) Health {
	worst := StateUnspecified
	for _, t := range tiers {
		if t.State > worst {
			worst = t.State
		}
	}
	return worst
}
