package main

import (
	"fmt"
	"testing"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/diagnose"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
)

func TestActionFromString(t *testing.T) {
	cases := map[string]fleet.Action{
		"none": fleet.ActionNone, "taint": fleet.ActionTaint, "cordon": fleet.ActionCordon,
		"drain": fleet.ActionDrain, "quarantine": fleet.ActionQuarantine,
		"DRAIN": fleet.ActionDrain, "garbage": fleet.ActionNone, " cordon ": fleet.ActionCordon,
	}
	for in, want := range cases {
		if got := actionFromString(in); got != want {
			t.Errorf("actionFromString(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestDecide_BudgetCapsLLMRecommendations is the pipeline's core safety
// property: even when the diagnoser recommends draining many nodes, the
// accountant admits only up to the ceiling and denies the rest.
func TestDecide_BudgetCapsLLMRecommendations(t *testing.T) {
	assessments := map[string]diagnose.Assessment{}
	for i := 0; i < 8; i++ {
		n := fmt.Sprintf("n%02d", i)
		assessments[n] = diagnose.Assessment{Node: n, FaultClass: "ecc_uncorrectable", RecommendedAction: "drain"}
	}
	// ceiling = floor(16 * 0.25) = 4
	quota := budget.Quota{MaxUnavailableRatio: 0.25, MinHealthyAbsolute: 1}
	decisions, _ := decide(assessments, nil, "r14", quota, 16)

	admitted, denied := 0, 0
	for _, d := range decisions {
		if d.Allowed {
			admitted++
		} else {
			denied++
		}
	}
	if admitted != 4 || denied != 4 {
		t.Errorf("admitted=%d denied=%d, want 4/4 (ceiling holds against 8 drain recommendations)", admitted, denied)
	}
}

func TestDecide_NonDisruptiveConsumesNoSlot(t *testing.T) {
	assessments := map[string]diagnose.Assessment{
		"n0": {Node: "n0", RecommendedAction: "taint"}, // non-disruptive
		"n1": {Node: "n1", RecommendedAction: "none"},
		"n2": {Node: "n2", RecommendedAction: "drain"},
	}
	quota := budget.Quota{MaxUnavailableRatio: 0.25, MinHealthyAbsolute: 1}
	decisions, actions := decide(assessments, nil, "r14", quota, 16)

	if _, ok := decisions["n0"]; ok {
		t.Error("taint is non-disruptive and must not appear as a budget decision")
	}
	if _, ok := decisions["n1"]; ok {
		t.Error("none must not appear as a budget decision")
	}
	if d, ok := decisions["n2"]; !ok || !d.Allowed {
		t.Error("the single drain should be admitted")
	}
	if actions["n0"] != fleet.ActionTaint || actions["n2"] != fleet.ActionDrain {
		t.Errorf("action mapping wrong: %v", actions)
	}
}
