package budget

import (
	"sync"
	"testing"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

const testClass = fleet.ClassName("h100-sxm-8x")

func fixedClock(t time.Time) Clock { return func() time.Time { return t } }

func intent(node string, action fleet.Action, origin fleet.Origin) fleet.Intent {
	return fleet.Intent{
		Node:   fleet.NodeID(node),
		Class:  testClass,
		Action: action,
		Origin: origin,
		Signal: "test-signal",
	}
}

func TestAdmit_NonDisruptiveAlwaysAllowed(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.0, MinHealthyAbsolute: 100},
	})
	// Fleet is already fully consumed and below the floor; a taint must still
	// be admitted because it evicts nothing.
	view := fleet.FleetView{Class: testClass, Total: 10, Unavailable: 10}

	d := a.Admit(intent("n1", fleet.ActionTaint, fleet.OriginHealth), view)
	if !d.Allowed {
		t.Fatalf("taint should always be admitted, denied: %s", d.Reason)
	}
	if a.InFlight(testClass) != 0 {
		t.Errorf("non-disruptive action must not consume a budget slot, got %d", a.InFlight(testClass))
	}
}

func TestAdmit_RatioCeiling(t *testing.T) {
	tests := []struct {
		name        string
		total       int
		unavailable int
		ratio       float64
		wantAllowed bool
	}{
		{"well under ceiling", 100, 0, 0.05, true},
		{"at ceiling minus one", 100, 4, 0.05, true},
		{"exactly at ceiling", 100, 5, 0.05, false},
		{"over ceiling", 100, 9, 0.05, false},
		// Floor semantics: 5% of 10 is 0.5, which floors to 0. Rounding up
		// would silently double the intended blast radius on small classes.
		{"small class floors to zero", 10, 0, 0.05, false},
		{"ratio permits exactly one", 20, 0, 0.05, true},
		{"full fleet drainable at 100%", 10, 9, 1.0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(map[fleet.ClassName]Quota{
				testClass: {MaxUnavailableRatio: tc.ratio, MinHealthyAbsolute: 0},
			})
			view := fleet.FleetView{Class: testClass, Total: tc.total, Unavailable: tc.unavailable}
			d := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view)
			if d.Allowed != tc.wantAllowed {
				t.Errorf("allowed = %v, want %v (reason: %s)", d.Allowed, tc.wantAllowed, d.Reason)
			}
		})
	}
}

func TestAdmit_AbsoluteHealthyFloor(t *testing.T) {
	// The ratio alone would allow these drains; the absolute floor must veto
	// the one that would drop the class below its minimum serving count.
	tests := []struct {
		name        string
		total       int
		unavailable int
		floor       int
		wantAllowed bool
	}{
		// 13 total, 1 drained leaves 12 serving, exactly at the floor: allowed.
		{"lands exactly on floor", 13, 0, 12, true},
		// 12 total, 1 drained leaves 11 serving, below the floor: denied.
		{"would breach floor", 12, 0, 12, false},
		{"already at floor", 13, 1, 12, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := New(map[fleet.ClassName]Quota{
				testClass: {MaxUnavailableRatio: 0.9, MinHealthyAbsolute: tc.floor},
			})
			view := fleet.FleetView{Class: testClass, Total: tc.total, Unavailable: tc.unavailable}

			d := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view)
			if d.Allowed != tc.wantAllowed {
				t.Fatalf("allowed = %v, want %v (reason: %s)", d.Allowed, tc.wantAllowed, d.Reason)
			}
			if !tc.wantAllowed && a.InFlight(testClass) != 0 {
				t.Errorf("denied intent must not consume a slot")
			}
		})
	}
}

// A stale view must not let callers over-admit. This is the single-threaded
// expression of the concurrency bug: the caller never refreshes Unavailable.
func TestAdmit_StaleViewDoesNotOverAdmit(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.1, MinHealthyAbsolute: 1},
	})
	stale := fleet.FleetView{Class: testClass, Total: 100, Unavailable: 0}

	admitted := 0
	for i := 0; i < 50; i++ {
		// Deliberately never update the view.
		if a.Admit(intent(nodeName(i), fleet.ActionDrain, fleet.OriginHealth), stale).Allowed {
			admitted++
		}
	}
	if admitted != 10 {
		t.Errorf("stale view should still cap at 10, got %d", admitted)
	}
}

// This is the scenario the whole architecture exists to prevent: a bad
// firmware push makes the entire fleet report unhealthy simultaneously.
func TestAdmit_MassFailureIsCappedNotAmplified(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.05, MinHealthyAbsolute: 1},
	})
	const total = 200
	view := fleet.FleetView{Class: testClass, Total: total, Unavailable: 0}

	admitted := 0
	for i := 0; i < total; i++ {
		d := a.Admit(intent(nodeName(i), fleet.ActionDrain, fleet.OriginHealth), view)
		if d.Allowed {
			admitted++
			view.Unavailable++
		}
	}

	if admitted != 10 {
		t.Fatalf("expected exactly 10 admissions (5%% of 200), got %d", admitted)
	}
	if a.InFlight(testClass) != 10 {
		t.Errorf("ledger should hold 10 slots, holds %d", a.InFlight(testClass))
	}
}

func TestAdmit_Idempotent(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.5, MinHealthyAbsolute: 1},
	})
	view := fleet.FleetView{Class: testClass, Total: 10, Unavailable: 0}

	first := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view)
	if !first.Allowed {
		t.Fatalf("first admit denied: %s", first.Reason)
	}
	// Reconcile loops are at-least-once, so this path is routine.
	for i := 0; i < 5; i++ {
		again := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view)
		if !again.Allowed {
			t.Fatalf("repeat admit %d denied: %s", i, again.Reason)
		}
	}
	if got := a.InFlight(testClass); got != 1 {
		t.Errorf("repeat admits must not double-count, in flight = %d, want 1", got)
	}
}

func TestAdmit_PlannedShareReservesHeadroom(t *testing.T) {
	// 20% of 100 = 20 slots. PlannedShare 0.5 means planned work gets 10.
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.2, MinHealthyAbsolute: 1, PlannedShare: 0.5},
	})
	view := fleet.FleetView{Class: testClass, Total: 100, Unavailable: 0}

	plannedAdmitted := 0
	for i := 0; i < 20; i++ {
		d := a.Admit(intent(nodeName(i), fleet.ActionDrain, fleet.OriginRollout), view)
		if d.Allowed {
			plannedAdmitted++
			view.Unavailable++
		}
	}
	if plannedAdmitted != 10 {
		t.Fatalf("planned work should be capped at 10, got %d", plannedAdmitted)
	}

	// Unplanned failure during the rollout must still find headroom: a dying
	// node does not wait for a maintenance window.
	d := a.Admit(intent("emergency", fleet.ActionDrain, fleet.OriginHealth), view)
	if !d.Allowed {
		t.Errorf("unplanned work must use reserved headroom, denied: %s", d.Reason)
	}
}

func TestAdmit_UnplannedMayUseWholeBudget(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.1, MinHealthyAbsolute: 1, PlannedShare: 0.5},
	})
	view := fleet.FleetView{Class: testClass, Total: 100, Unavailable: 0}

	admitted := 0
	for i := 0; i < 20; i++ {
		if a.Admit(intent(nodeName(i), fleet.ActionDrain, fleet.OriginHealth), view).Allowed {
			admitted++
			view.Unavailable++
		}
	}
	if admitted != 10 {
		t.Errorf("unplanned work should reach the full budget of 10, got %d", admitted)
	}
}

func TestRelease_FreesSlot(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.1, MinHealthyAbsolute: 1},
	})
	view := fleet.FleetView{Class: testClass, Total: 10, Unavailable: 0}

	if !a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view).Allowed {
		t.Fatal("setup admit failed")
	}
	if !a.Holds(testClass, "n1") {
		t.Fatal("expected n1 to hold a slot")
	}

	a.Release(testClass, "n1")

	if a.Holds(testClass, "n1") {
		t.Error("slot should be released")
	}
	if a.InFlight(testClass) != 0 {
		t.Errorf("in flight should be 0, got %d", a.InFlight(testClass))
	}
}

func TestReap_ReclaimsStaleSlots(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	clock := now
	a := New(
		map[fleet.ClassName]Quota{testClass: {MaxUnavailableRatio: 0.5, MinHealthyAbsolute: 1}},
		WithClock(func() time.Time { return clock }),
		WithStaleTimeout(30*time.Minute),
	)
	view := fleet.FleetView{Class: testClass, Total: 10, Unavailable: 0}

	if !a.Admit(intent("crashed", fleet.ActionDrain, fleet.OriginHealth), view).Allowed {
		t.Fatal("setup admit failed")
	}

	// Simulate the controller crashing mid-drain: the slot is never released.
	clock = now.Add(31 * time.Minute)

	if got := a.InFlight(testClass); got != 0 {
		t.Errorf("stale slot should be reaped, in flight = %d", got)
	}
}

func TestAdmit_EmptyClassDenied(t *testing.T) {
	a := New(nil)
	d := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth),
		fleet.FleetView{Class: testClass, Total: 0, Unavailable: 0})
	if d.Allowed {
		t.Error("draining a node in an empty class should be denied")
	}
}

func TestDefaultQuota_AppliedForUnknownClass(t *testing.T) {
	a := New(nil)
	q := a.Quota("never-configured")
	if q.MaxUnavailableRatio != 0.05 {
		t.Errorf("default ratio = %v, want 0.05", q.MaxUnavailableRatio)
	}
	if q.MinHealthyAbsolute != 1 {
		t.Errorf("default floor = %v, want 1", q.MinHealthyAbsolute)
	}
}

func TestAdmit_ConcurrentSafety(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.1, MinHealthyAbsolute: 1},
	})
	// Every goroutine sees the same zero-unavailable view, which is the
	// pathological case: without the ledger, all 100 would be admitted.
	view := fleet.FleetView{Class: testClass, Total: 100, Unavailable: 0}

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if a.Admit(intent(nodeName(i), fleet.ActionDrain, fleet.OriginHealth), view).Allowed {
				mu.Lock()
				admitted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if admitted != 10 {
		t.Errorf("concurrent admits should still cap at 10, got %d", admitted)
	}
}

func TestDecision_CarriesBudgetForAudit(t *testing.T) {
	a := New(map[fleet.ClassName]Quota{
		testClass: {MaxUnavailableRatio: 0.5, MinHealthyAbsolute: 1},
	})
	view := fleet.FleetView{Class: testClass, Total: 10, Unavailable: 3}

	d := a.Admit(intent("n1", fleet.ActionDrain, fleet.OriginHealth), view)
	if !d.Allowed {
		t.Fatalf("unexpected denial: %s", d.Reason)
	}
	if d.BudgetBefore != 3 || d.BudgetAfter != 4 {
		t.Errorf("budget before/after = %d/%d, want 3/4", d.BudgetBefore, d.BudgetAfter)
	}
	if d.Reason == "" {
		t.Error("reason must be populated even on allow, for the audit record")
	}
}

func nodeName(i int) string {
	return "node-" + string(rune('a'+i%26)) + "-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
