// Package policy turns signal breaches into disruption intents.
//
// A node may breach several signals at once. Rather than emitting one intent
// per breach — which would produce conflicting actions and a noisy audit trail
// — the evaluator collapses them into the single most urgent intent, ordered
// first by severity and then by action rank.
package policy

import (
	"sort"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

// Evaluator collapses breaches into intents.
type Evaluator struct{}

// New returns a policy evaluator.
func New() *Evaluator { return &Evaluator{} }

// Evaluate produces at most one intent per node.
//
// Nodes already in a terminal state are skipped: a quarantined node has
// already been removed from service, and re-emitting intents for it would
// churn the budget ledger without changing anything.
func (e *Evaluator) Evaluate(
	nodes map[fleet.NodeID]fleet.NodeSnapshot,
	breaches []signal.Breach,
	now time.Time,
) []fleet.Intent {

	byNode := make(map[fleet.NodeID][]signal.Breach)
	for _, b := range breaches {
		byNode[b.Node] = append(byNode[b.Node], b)
	}

	// Stable iteration so output ordering is deterministic.
	ids := make([]fleet.NodeID, 0, len(byNode))
	for id := range byNode {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var intents []fleet.Intent
	for _, id := range ids {
		snap, known := nodes[id]
		if !known {
			continue
		}
		if snap.State == fleet.StateQuarantined {
			continue
		}

		winner, ok := mostUrgent(byNode[id])
		if !ok {
			continue
		}

		// Never de-escalate. If a node is already draining, a Warning-level
		// cordon intent must not walk it backwards.
		if winner.Action.Rank() <= currentActionRank(snap.State) {
			continue
		}

		intents = append(intents, fleet.Intent{
			Node:        id,
			Class:       snap.Class,
			Action:      winner.Action,
			Origin:      fleet.OriginHealth,
			Signal:      winner.Signal,
			Severity:    winner.Severity,
			Observed:    winner.Observed,
			RequestedAt: now,
		})
	}
	return intents
}

// mostUrgent picks the breach that should drive the node's action.
func mostUrgent(bs []signal.Breach) (signal.Breach, bool) {
	if len(bs) == 0 {
		return signal.Breach{}, false
	}
	best := bs[0]
	for _, b := range bs[1:] {
		if b.Severity.Rank() > best.Severity.Rank() {
			best = b
			continue
		}
		if b.Severity.Rank() == best.Severity.Rank() {
			if b.Action.Rank() > best.Action.Rank() {
				best = b
				continue
			}
			// Deterministic tie-break so repeated reconciles of identical input
			// produce identical output.
			if b.Action.Rank() == best.Action.Rank() && b.Signal < best.Signal {
				best = b
			}
		}
	}
	return best, true
}

// currentActionRank maps a node's state back to the action rank that produced
// it, so the evaluator can refuse to de-escalate.
func currentActionRank(s fleet.State) int {
	switch s {
	case fleet.StateQuarantined:
		return fleet.ActionQuarantine.Rank()
	case fleet.StateDraining:
		return fleet.ActionDrain.Rank()
	case fleet.StateCordoned:
		return fleet.ActionCordon.Rank()
	}
	return 0
}
