// Package fleet defines the core domain vocabulary for fleetwarden.
//
// This package intentionally has zero dependencies outside the standard
// library. Every other internal package builds on these types, which is what
// allows the health, budget and lifecycle logic to be exercised in unit tests
// without a Kubernetes API server.
package fleet

import (
	"fmt"
	"time"
)

// NodeID is the cluster-unique name of a node.
type NodeID string

// ClassName identifies a NodeClass (a hardware SKU grouping).
type ClassName string

// State is a node's position in the lifecycle state machine.
type State string

const (
	// StateHealthy is the steady state: schedulable, no active breaches.
	StateHealthy State = "Healthy"
	// StateSuspect means at least one signal is breaching but has not yet
	// persisted long enough to justify disruption.
	StateSuspect State = "Suspect"
	// StateCordoned means the node is unschedulable but existing workloads
	// are untouched.
	StateCordoned State = "Cordoned"
	// StateDraining means workloads are actively being evicted.
	StateDraining State = "Draining"
	// StateQuarantined is a terminal state pending human or automated repair.
	StateQuarantined State = "Quarantined"
)

// Valid reports whether s is a recognised state.
func (s State) Valid() bool {
	switch s {
	case StateHealthy, StateSuspect, StateCordoned, StateDraining, StateQuarantined:
		return true
	}
	return false
}

// Severity ranks how urgent a signal breach is.
type Severity string

const (
	SeverityInfo     Severity = "Info"
	SeverityWarning  Severity = "Warning"
	SeverityCritical Severity = "Critical"
)

// Rank returns a total ordering for severities so the policy evaluator can
// pick the most urgent of several concurrent breaches.
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarning:
		return 2
	case SeverityInfo:
		return 1
	}
	return 0
}

// Action is a remediation step requested by policy.
type Action string

const (
	// ActionNone is a no-op, used when no signal is breaching.
	ActionNone Action = "None"
	// ActionTaint marks a node without disrupting workloads. Non-disruptive:
	// it never consumes disruption budget.
	ActionTaint Action = "Taint"
	// ActionCordon makes a node unschedulable. Disruptive.
	ActionCordon Action = "Cordon"
	// ActionDrain evicts workloads. Disruptive.
	ActionDrain Action = "Drain"
	// ActionQuarantine drains and holds the node out of service. Disruptive.
	ActionQuarantine Action = "Quarantine"
)

// Disruptive reports whether an action removes capacity from the fleet and
// therefore must be admitted by the budget accountant.
//
// Taint is deliberately non-disruptive: it influences future scheduling but
// evicts nothing, so gating it on budget would stall drift detection during
// exactly the incidents where drift matters most.
func (a Action) Disruptive() bool {
	switch a {
	case ActionCordon, ActionDrain, ActionQuarantine:
		return true
	}
	return false
}

// Rank orders actions by escalation severity so the state machine only ever
// moves forward.
func (a Action) Rank() int {
	switch a {
	case ActionQuarantine:
		return 4
	case ActionDrain:
		return 3
	case ActionCordon:
		return 2
	case ActionTaint:
		return 1
	}
	return 0
}

// Origin records which subsystem requested a disruption. Used for budget
// share accounting between planned and unplanned work.
type Origin string

const (
	// OriginHealth is unplanned remediation driven by signal breaches.
	OriginHealth Origin = "Health"
	// OriginRollout is a firmware or driver rollout wave.
	OriginRollout Origin = "Rollout"
	// OriginMaintenance is planned maintenance inside a window.
	OriginMaintenance Origin = "Maintenance"
	// OriginSizing is a pool downsize.
	OriginSizing Origin = "Sizing"
)

// Planned reports whether the origin represents scheduled work, which draws
// from the reserved maintenance share rather than the general budget.
func (o Origin) Planned() bool {
	return o == OriginRollout || o == OriginMaintenance || o == OriginSizing
}

// NodeSnapshot is an immutable view of one node at reconcile time.
type NodeSnapshot struct {
	ID          NodeID
	Class       ClassName
	State       State
	Schedulable bool
	// EnteredState is when the node last transitioned. The state machine uses
	// this to enforce dwell times.
	EnteredState time.Time
	// Ready reflects the kubelet's own view, independent of our state machine.
	Ready bool
}

// FleetView is the aggregate state the budget accountant reasons over. It is
// computed once per reconcile and passed by value so admission decisions are
// made against a consistent snapshot.
type FleetView struct {
	Class ClassName
	// Total is every node in the class, regardless of state.
	Total int
	// Unavailable counts nodes not currently serving workloads.
	Unavailable int
}

// Healthy returns the count of nodes currently serving workloads.
func (f FleetView) Healthy() int {
	h := f.Total - f.Unavailable
	if h < 0 {
		return 0
	}
	return h
}

// Intent is a request to take an action against a node. Modules never mutate
// nodes directly; they emit intents which the budget accountant admits or
// denies. This indirection is what bounds blast radius by construction.
type Intent struct {
	Node   NodeID
	Class  ClassName
	Action Action
	Origin Origin
	// Signal names the rule that triggered this intent, so every action in the
	// audit log is attributable.
	Signal   string
	Severity Severity
	// Observed is the value that breached, retained for the audit record.
	Observed    string
	RequestedAt time.Time
}

func (i Intent) String() string {
	return fmt.Sprintf("%s/%s %s (signal=%q severity=%s origin=%s)",
		i.Class, i.Node, i.Action, i.Signal, i.Severity, i.Origin)
}
