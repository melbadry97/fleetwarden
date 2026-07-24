// Package lifecycle implements the node state machine.
//
//	Healthy ──(breach)──▶ Suspect ──(persists past grace)──▶ Cordoned
//	   ▲                     │                                   │
//	   │              (recovers)                          (grace elapsed)
//	   │                     │                                   ▼
//	   └──(manual clear)── Quarantined ◀──(drain done)──── Draining
//
// Escalation is progressive and every transition is attributable. Taint does not
// move the machine: it is an annotation on a node that remains in service.
package lifecycle

import (
	"fmt"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

// Config tunes dwell times.
type Config struct {
	// GracePeriod is how long a node must dwell in a state before escalating.
	GracePeriod time.Duration
	// DrainTimeout bounds how long Draining may last before the node is
	// forced to Quarantined. Without this a pod with a broken PDB pins a
	// budget slot forever.
	DrainTimeout time.Duration
}

// DefaultConfig returns conservative dwell times.
func DefaultConfig() Config {
	return Config{GracePeriod: 5 * time.Minute, DrainTimeout: 15 * time.Minute}
}

// Transition describes a state change the machine wants to make.
type Transition struct {
	Node   fleet.NodeID
	From   fleet.State
	To     fleet.State
	Reason string
	// ReleasesBudget is true when the target state no longer consumes a
	// disruption slot, so the caller knows to release the ledger entry.
	ReleasesBudget bool
	// RequiresBudget is true when the target state consumes a slot and the
	// transition must therefore be admitted before it is applied.
	RequiresBudget bool
}

func (t Transition) String() string {
	return fmt.Sprintf("%s: %s -> %s (%s)", t.Node, t.From, t.To, t.Reason)
}

// Machine computes state transitions. It is pure: it reads a snapshot and an
// optional intent and returns what should happen, performing no I/O. That is
// what makes it exhaustively testable.
type Machine struct{ cfg Config }

// NewMachine builds a Machine.
func NewMachine(cfg Config) *Machine {
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = DefaultConfig().GracePeriod
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultConfig().DrainTimeout
	}
	return &Machine{cfg: cfg}
}

// DrainStatus reports progress of an in-flight drain, supplied by the caller
// because counting pods requires a Kubernetes client.
type DrainStatus struct {
	InProgress bool
	Complete   bool
}

// Next computes the transition for a node, or false if it should stay put.
//
// intent may be nil, meaning no signal is currently breaching for this node.
// cleared indicates an operator has manually cleared a quarantine.
func (m *Machine) Next(
	snap fleet.NodeSnapshot,
	intent *fleet.Intent,
	drain DrainStatus,
	cleared bool,
	now time.Time,
) (Transition, bool) {

	dwell := now.Sub(snap.EnteredState)

	switch snap.State {

	case fleet.StateHealthy:
		if intent == nil {
			return Transition{}, false
		}
		// A Critical breach on a healthy node still passes through Suspect.
		// The window and hysteresis in the signal evaluator already guarded
		// against noise; this second dwell guards against a policy that was
		// misconfigured with a zero window.
		if intent.Action.Disruptive() || intent.Action == fleet.ActionTaint {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateSuspect,
				Reason: fmt.Sprintf("signal %q breached (severity=%s)", intent.Signal, intent.Severity),
			}, true
		}
		return Transition{}, false

	case fleet.StateSuspect:
		// Recovery: the breach cleared before the grace period elapsed.
		if intent == nil {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateHealthy,
				Reason: "signals recovered before escalation",
			}, true
		}
		// A non-disruptive action holds the node in Suspect indefinitely: it
		// is flagged, but nothing is evicted.
		if !intent.Action.Disruptive() {
			return Transition{}, false
		}
		if dwell < m.cfg.GracePeriod {
			return Transition{}, false
		}
		return Transition{
			Node: snap.ID, From: snap.State, To: fleet.StateCordoned,
			Reason:         fmt.Sprintf("signal %q persisted past grace period", intent.Signal),
			RequiresBudget: true,
		}, true

	case fleet.StateCordoned:
		// Recovery from cordon returns the node to service and frees budget.
		if intent == nil {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateHealthy,
				Reason: "signals recovered while cordoned", ReleasesBudget: true,
			}, true
		}
		// Cordon is the terminal response to a Cordon-level intent; only a
		// Drain-or-higher intent escalates further.
		if intent.Action.Rank() <= fleet.ActionCordon.Rank() {
			return Transition{}, false
		}
		if dwell < m.cfg.GracePeriod {
			return Transition{}, false
		}
		return Transition{
			Node: snap.ID, From: snap.State, To: fleet.StateDraining,
			Reason: fmt.Sprintf("escalating to drain on signal %q", intent.Signal),
		}, true

	case fleet.StateDraining:
		if drain.Complete {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateQuarantined,
				Reason: "drain complete",
			}, true
		}
		// A stuck drain must not hold a budget slot forever. Force the node
		// terminal and let a human sort out the pod that would not evict.
		if dwell >= m.cfg.DrainTimeout {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateQuarantined,
				Reason: fmt.Sprintf("drain exceeded timeout of %s", m.cfg.DrainTimeout),
			}, true
		}
		return Transition{}, false

	case fleet.StateQuarantined:
		// Quarantine is deliberately not self-clearing. A node that failed
		// hard enough to be quarantined should be inspected, and automatic
		// return to service would mask a recurring hardware fault.
		if cleared {
			return Transition{
				Node: snap.ID, From: snap.State, To: fleet.StateHealthy,
				Reason: "quarantine manually cleared", ReleasesBudget: true,
			}, true
		}
		return Transition{}, false
	}

	// Unknown or empty state: adopt the node as Healthy so a malformed
	// annotation cannot wedge reconciliation.
	return Transition{
		Node: snap.ID, From: snap.State, To: fleet.StateHealthy,
		Reason: fmt.Sprintf("adopting node from unrecognised state %q", snap.State),
	}, true
}

// Config returns the machine's configuration.
func (m *Machine) Config() Config { return m.cfg }
