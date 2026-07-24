package lifecycle

import (
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

var t0 = time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)

func snap(state fleet.State, entered time.Time) fleet.NodeSnapshot {
	return fleet.NodeSnapshot{
		ID: "n1", Class: "h100", State: state, EnteredState: entered, Ready: true, Schedulable: true,
	}
}

func drainIntent() *fleet.Intent {
	return &fleet.Intent{
		Node: "n1", Class: "h100", Action: fleet.ActionDrain,
		Origin: fleet.OriginHealth, Signal: "eccUncorrectable", Severity: fleet.SeverityCritical,
	}
}

func cordonIntent() *fleet.Intent {
	return &fleet.Intent{
		Node: "n1", Class: "h100", Action: fleet.ActionCordon,
		Origin: fleet.OriginHealth, Signal: "thermalThrottle", Severity: fleet.SeverityWarning,
	}
}

func taintIntent() *fleet.Intent {
	return &fleet.Intent{
		Node: "n1", Class: "h100", Action: fleet.ActionTaint,
		Origin: fleet.OriginHealth, Signal: "firmwareDrift", Severity: fleet.SeverityInfo,
	}
}

func TestTransitions(t *testing.T) {
	cfg := Config{GracePeriod: 5 * time.Minute, DrainTimeout: 15 * time.Minute}
	m := NewMachine(cfg)

	tests := []struct {
		name     string
		snap     fleet.NodeSnapshot
		intent   *fleet.Intent
		drain    DrainStatus
		cleared  bool
		now      time.Time
		wantMove bool
		wantTo   fleet.State
	}{
		{
			name: "healthy with no breach stays put",
			snap: snap(fleet.StateHealthy, t0), intent: nil, now: t0,
			wantMove: false,
		},
		{
			name: "healthy with breach becomes suspect",
			snap: snap(fleet.StateHealthy, t0), intent: drainIntent(), now: t0,
			wantMove: true, wantTo: fleet.StateSuspect,
		},
		{
			name: "suspect recovers before grace elapses",
			snap: snap(fleet.StateSuspect, t0), intent: nil, now: t0.Add(time.Minute),
			wantMove: true, wantTo: fleet.StateHealthy,
		},
		{
			name: "suspect holds inside grace period",
			snap: snap(fleet.StateSuspect, t0), intent: drainIntent(), now: t0.Add(4 * time.Minute),
			wantMove: false,
		},
		{
			name: "suspect escalates to cordon after grace",
			snap: snap(fleet.StateSuspect, t0), intent: drainIntent(), now: t0.Add(6 * time.Minute),
			wantMove: true, wantTo: fleet.StateCordoned,
		},
		{
			// Taint flags a node without removing it from service, so it must
			// never escalate past Suspect on its own.
			name: "taint intent never escalates past suspect",
			snap: snap(fleet.StateSuspect, t0), intent: taintIntent(), now: t0.Add(time.Hour),
			wantMove: false,
		},
		{
			name: "cordoned recovers and returns to service",
			snap: snap(fleet.StateCordoned, t0), intent: nil, now: t0.Add(time.Minute),
			wantMove: true, wantTo: fleet.StateHealthy,
		},
		{
			// Cordon is the terminal response to a Cordon-level intent.
			name: "cordon intent does not escalate to drain",
			snap: snap(fleet.StateCordoned, t0), intent: cordonIntent(), now: t0.Add(time.Hour),
			wantMove: false,
		},
		{
			name: "drain intent escalates cordoned to draining after grace",
			snap: snap(fleet.StateCordoned, t0), intent: drainIntent(), now: t0.Add(6 * time.Minute),
			wantMove: true, wantTo: fleet.StateDraining,
		},
		{
			name: "draining waits while pods evict",
			snap: snap(fleet.StateDraining, t0), intent: drainIntent(),
			drain: DrainStatus{InProgress: true}, now: t0.Add(time.Minute),
			wantMove: false,
		},
		{
			name: "draining completes to quarantined",
			snap: snap(fleet.StateDraining, t0), intent: drainIntent(),
			drain: DrainStatus{Complete: true}, now: t0.Add(2 * time.Minute),
			wantMove: true, wantTo: fleet.StateQuarantined,
		},
		{
			// A pod with a broken PDB would otherwise pin a budget slot forever.
			name: "stuck drain is forced terminal at timeout",
			snap: snap(fleet.StateDraining, t0), intent: drainIntent(),
			drain: DrainStatus{InProgress: true}, now: t0.Add(16 * time.Minute),
			wantMove: true, wantTo: fleet.StateQuarantined,
		},
		{
			// Automatic return to service would mask a recurring hardware fault.
			name: "quarantine does not self-clear even when signals recover",
			snap: snap(fleet.StateQuarantined, t0), intent: nil, now: t0.Add(24 * time.Hour),
			wantMove: false,
		},
		{
			name: "quarantine clears on manual intervention",
			snap: snap(fleet.StateQuarantined, t0), intent: nil, cleared: true, now: t0.Add(time.Hour),
			wantMove: true, wantTo: fleet.StateHealthy,
		},
		{
			name: "unrecognised state is adopted as healthy",
			snap: snap(fleet.State("Gibberish"), t0), intent: nil, now: t0,
			wantMove: true, wantTo: fleet.StateHealthy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr, moved := m.Next(tc.snap, tc.intent, tc.drain, tc.cleared, tc.now)
			if moved != tc.wantMove {
				t.Fatalf("moved = %v, want %v (transition: %+v)", moved, tc.wantMove, tr)
			}
			if !moved {
				return
			}
			if tr.To != tc.wantTo {
				t.Errorf("To = %s, want %s", tr.To, tc.wantTo)
			}
			if tr.From != tc.snap.State {
				t.Errorf("From = %s, want %s", tr.From, tc.snap.State)
			}
			if tr.Reason == "" {
				t.Error("every transition must carry a reason for the audit trail")
			}
			if tr.Node != tc.snap.ID {
				t.Errorf("Node = %s, want %s", tr.Node, tc.snap.ID)
			}
		})
	}
}

func TestTransition_BudgetSignalling(t *testing.T) {
	m := NewMachine(DefaultConfig())

	// Entering Cordoned removes capacity and must be gated on budget.
	tr, moved := m.Next(snap(fleet.StateSuspect, t0), drainIntent(), DrainStatus{}, false, t0.Add(10*time.Minute))
	if !moved || tr.To != fleet.StateCordoned {
		t.Fatalf("setup: expected escalation to Cordoned, got %+v moved=%v", tr, moved)
	}
	if !tr.RequiresBudget {
		t.Error("entering Cordoned must require budget admission")
	}

	// Returning to service frees the slot.
	tr, moved = m.Next(snap(fleet.StateCordoned, t0), nil, DrainStatus{}, false, t0.Add(time.Minute))
	if !moved || tr.To != fleet.StateHealthy {
		t.Fatalf("setup: expected recovery, got %+v moved=%v", tr, moved)
	}
	if !tr.ReleasesBudget {
		t.Error("recovery from Cordoned must release the budget slot")
	}
}

func TestNewMachine_RejectsZeroConfig(t *testing.T) {
	m := NewMachine(Config{})
	if m.Config().GracePeriod <= 0 {
		t.Error("zero grace period should fall back to the default")
	}
	if m.Config().DrainTimeout <= 0 {
		t.Error("zero drain timeout should fall back to the default")
	}
}

// A full escalation walk, which is the path an operator actually observes
// during an incident.
func TestFullEscalationPath(t *testing.T) {
	m := NewMachine(Config{GracePeriod: 5 * time.Minute, DrainTimeout: 15 * time.Minute})

	cur := snap(fleet.StateHealthy, t0)
	now := t0
	want := []fleet.State{
		fleet.StateSuspect,
		fleet.StateCordoned,
		fleet.StateDraining,
		fleet.StateQuarantined,
	}

	for i, expect := range want {
		var drain DrainStatus
		if expect == fleet.StateQuarantined {
			drain = DrainStatus{Complete: true}
		}
		tr, moved := m.Next(cur, drainIntent(), drain, false, now)
		if !moved {
			t.Fatalf("step %d: expected transition to %s, machine stayed in %s", i, expect, cur.State)
		}
		if tr.To != expect {
			t.Fatalf("step %d: moved to %s, want %s", i, tr.To, expect)
		}
		// The node enters the new state at the moment of transition; only
		// then does the grace period begin to elapse.
		cur = snap(tr.To, now)
		now = now.Add(6 * time.Minute)
	}
}
