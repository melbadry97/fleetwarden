package policy

import (
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

var now = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func nodes(states map[fleet.NodeID]fleet.State) map[fleet.NodeID]fleet.NodeSnapshot {
	out := make(map[fleet.NodeID]fleet.NodeSnapshot, len(states))
	for id, st := range states {
		out[id] = fleet.NodeSnapshot{ID: id, Class: "h100", State: st, EnteredState: now}
	}
	return out
}

func breach(node fleet.NodeID, name string, sev fleet.Severity, act fleet.Action) signal.Breach {
	return signal.Breach{Node: node, Signal: name, Severity: sev, Action: act, Observed: "9"}
}

func TestEvaluate_CollapsesToMostUrgent(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{"n1": fleet.StateHealthy})

	bs := []signal.Breach{
		breach("n1", "firmwareDrift", fleet.SeverityInfo, fleet.ActionTaint),
		breach("n1", "eccUncorrectable", fleet.SeverityCritical, fleet.ActionDrain),
		breach("n1", "thermalThrottle", fleet.SeverityWarning, fleet.ActionCordon),
	}

	got := e.Evaluate(n, bs, now)
	if len(got) != 1 {
		t.Fatalf("expected exactly one intent per node, got %d", len(got))
	}
	if got[0].Action != fleet.ActionDrain {
		t.Errorf("action = %s, want Drain (highest severity wins)", got[0].Action)
	}
	if got[0].Signal != "eccUncorrectable" {
		t.Errorf("signal = %q, want the triggering signal to be attributable", got[0].Signal)
	}
	if got[0].Origin != fleet.OriginHealth {
		t.Errorf("origin = %s, want Health", got[0].Origin)
	}
}

func TestEvaluate_TieBreaksDeterministically(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{"n1": fleet.StateHealthy})

	// Same severity and same action rank: the tie-break must be stable so
	// repeated reconciles of identical input produce identical output.
	bs := []signal.Breach{
		breach("n1", "zeta", fleet.SeverityCritical, fleet.ActionDrain),
		breach("n1", "alpha", fleet.SeverityCritical, fleet.ActionDrain),
	}

	for i := 0; i < 20; i++ {
		got := e.Evaluate(n, bs, now)
		if len(got) != 1 || got[0].Signal != "alpha" {
			t.Fatalf("iteration %d: unstable tie-break, got %+v", i, got)
		}
	}
}

func TestEvaluate_NeverDeEscalates(t *testing.T) {
	e := New()

	tests := []struct {
		name      string
		state     fleet.State
		action    fleet.Action
		wantCount int
	}{
		{"cordon intent on cordoned node is redundant", fleet.StateCordoned, fleet.ActionCordon, 0},
		{"taint intent on draining node walks backwards", fleet.StateDraining, fleet.ActionTaint, 0},
		{"drain intent on cordoned node escalates", fleet.StateCordoned, fleet.ActionDrain, 1},
		{"drain intent on healthy node escalates", fleet.StateHealthy, fleet.ActionDrain, 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			n := nodes(map[fleet.NodeID]fleet.State{"n1": tc.state})
			bs := []signal.Breach{breach("n1", "s", fleet.SeverityCritical, tc.action)}
			got := e.Evaluate(n, bs, now)
			if len(got) != tc.wantCount {
				t.Errorf("got %d intents, want %d (%+v)", len(got), tc.wantCount, got)
			}
		})
	}
}

func TestEvaluate_SkipsQuarantinedNodes(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{"n1": fleet.StateQuarantined})
	bs := []signal.Breach{breach("n1", "ecc", fleet.SeverityCritical, fleet.ActionQuarantine)}

	if got := e.Evaluate(n, bs, now); len(got) != 0 {
		t.Errorf("quarantined nodes should emit no intents, got %+v", got)
	}
}

func TestEvaluate_SkipsUnknownNodes(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{"n1": fleet.StateHealthy})
	// A breach for a node that has left the cluster between scrape and reconcile.
	bs := []signal.Breach{breach("ghost", "ecc", fleet.SeverityCritical, fleet.ActionDrain)}

	if got := e.Evaluate(n, bs, now); len(got) != 0 {
		t.Errorf("unknown nodes should be skipped, got %+v", got)
	}
}

func TestEvaluate_OutputIsSortedByNode(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{
		"n1": fleet.StateHealthy, "n2": fleet.StateHealthy, "n3": fleet.StateHealthy,
	})
	bs := []signal.Breach{
		breach("n3", "ecc", fleet.SeverityCritical, fleet.ActionDrain),
		breach("n1", "ecc", fleet.SeverityCritical, fleet.ActionDrain),
		breach("n2", "ecc", fleet.SeverityCritical, fleet.ActionDrain),
	}

	got := e.Evaluate(n, bs, now)
	if len(got) != 3 {
		t.Fatalf("want 3 intents, got %d", len(got))
	}
	for i, want := range []fleet.NodeID{"n1", "n2", "n3"} {
		if got[i].Node != want {
			t.Errorf("position %d = %s, want %s", i, got[i].Node, want)
		}
	}
}

func TestEvaluate_NoBreachesNoIntents(t *testing.T) {
	e := New()
	n := nodes(map[fleet.NodeID]fleet.State{"n1": fleet.StateHealthy})
	if got := e.Evaluate(n, nil, now); len(got) != 0 {
		t.Errorf("no breaches should produce no intents, got %+v", got)
	}
}
