package fleet

import "testing"

func TestAction_Disruptive(t *testing.T) {
	tests := map[Action]bool{
		ActionNone:       false,
		ActionTaint:      false, // flags a node but evicts nothing
		ActionCordon:     true,
		ActionDrain:      true,
		ActionQuarantine: true,
	}
	for action, want := range tests {
		if got := action.Disruptive(); got != want {
			t.Errorf("%s.Disruptive() = %v, want %v", action, got, want)
		}
	}
}

func TestAction_RankIsStrictlyOrdered(t *testing.T) {
	order := []Action{ActionNone, ActionTaint, ActionCordon, ActionDrain, ActionQuarantine}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Errorf("%s (rank %d) must outrank %s (rank %d)",
				order[i], order[i].Rank(), order[i-1], order[i-1].Rank())
		}
	}
}

func TestSeverity_RankIsStrictlyOrdered(t *testing.T) {
	order := []Severity{SeverityInfo, SeverityWarning, SeverityCritical}
	for i := 1; i < len(order); i++ {
		if order[i].Rank() <= order[i-1].Rank() {
			t.Errorf("%s must outrank %s", order[i], order[i-1])
		}
	}
	if Severity("nonsense").Rank() != 0 {
		t.Error("unknown severity should rank zero so it never wins a comparison")
	}
}

func TestOrigin_Planned(t *testing.T) {
	tests := map[Origin]bool{
		OriginHealth:      false, // a dying node does not wait for a window
		OriginRollout:     true,
		OriginMaintenance: true,
		OriginSizing:      true,
	}
	for origin, want := range tests {
		if got := origin.Planned(); got != want {
			t.Errorf("%s.Planned() = %v, want %v", origin, got, want)
		}
	}
}

func TestState_Valid(t *testing.T) {
	valid := []State{StateHealthy, StateSuspect, StateCordoned, StateDraining, StateQuarantined}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%s should be a valid state", s)
		}
	}
	if State("Gibberish").Valid() {
		t.Error("unrecognised state should not validate")
	}
}

func TestFleetView_Healthy(t *testing.T) {
	tests := []struct {
		name        string
		total       int
		unavailable int
		want        int
	}{
		{"nominal", 100, 5, 95},
		{"all down", 10, 10, 0},
		// Defensive: a miscounted view must not produce a negative that would
		// wrap comparisons elsewhere.
		{"over-counted unavailable clamps to zero", 10, 15, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := FleetView{Total: tc.total, Unavailable: tc.unavailable}
			if got := v.Healthy(); got != tc.want {
				t.Errorf("Healthy() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestIntent_StringIsAttributable(t *testing.T) {
	i := Intent{
		Node: "n1", Class: "h100", Action: ActionDrain,
		Origin: OriginHealth, Signal: "eccUncorrectable", Severity: SeverityCritical,
	}
	s := i.String()
	for _, want := range []string{"n1", "h100", "Drain", "eccUncorrectable", "Critical", "Health"} {
		if !contains(s, want) {
			t.Errorf("Intent.String() = %q, missing %q", s, want)
		}
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
