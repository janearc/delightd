package model

import (
	"fmt"
	"os"
	"time"
)

// The model-health ladder (docs/model-hosting.md §4): a model's "green" is not one bit
// but a set of rungs, cheap -> expensive, reported in the fleet GREEN/YELLOW/RED
// vocabulary. The always-on tiers here -- declared and reachable -- are the continuous
// signal; the loadable and "has-a-brain" tiers are summoned (they cost a load) and land in
// a following step.

type HealthTier string

const (
	TierDeclared  HealthTier = "declared"  // the manifest declares it and its weights are present
	TierReachable HealthTier = "reachable" // its endpoint answers
)

type TierState string

const (
	StateGreen   TierState = "GREEN"
	StateYellow  TierState = "YELLOW"
	StateRed     TierState = "RED"
	StateUnknown TierState = "UNKNOWN"
)

// TierResult is one rung's verdict.
type TierResult struct {
	Tier   HealthTier `json:"tier"`
	State  TierState  `json:"state"`
	Detail string     `json:"detail,omitempty"`
}

// LadderReport is the per-deployment roll-up: the assessed rungs and a worst-wins overall.
type LadderReport struct {
	Deployment string       `json:"deployment"`
	Backend    Backend      `json:"backend"`
	Overall    TierState    `json:"overall"`
	Tiers      []TierResult `json:"tiers"`
}

// tierDeclared checks the weights are where the descriptor says. A path / HF-cache
// location is stat-ed; an ollama tag is a backend handle (not a path), so its
// registration check belongs to the later loadable tier -- here it is GREEN as a coherent
// declaration.
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

// worstState returns the worst rung's state: RED > YELLOW > GREEN > UNKNOWN.
func worstState(tiers []TierResult) TierState {
	rank := map[TierState]int{StateUnknown: 0, StateGreen: 1, StateYellow: 2, StateRed: 3}
	worst := StateUnknown
	for _, t := range tiers {
		if rank[t.State] > rank[worst] {
			worst = t.State
		}
	}
	return worst
}
