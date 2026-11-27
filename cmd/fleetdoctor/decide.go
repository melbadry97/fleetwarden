package main

import (
	"sort"
	"strings"
	"time"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/diagnose"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

// decide routes each assessment's recommended action through the real budget
// accountant. This is the pipeline's safety property: the LLM advises, the
// deterministic accountant decides. A recommendation becomes a fleet.Intent and
// is admitted or denied, so blast radius stays bounded no matter what the model
// recommends.
//
// Nodes are processed in sorted order for a deterministic admit sequence.
// Non-disruptive actions (none, taint) consume no slot; only disruptive ones
// appear in the returned decisions map.
func decide(
	assessments map[string]diagnose.Assessment,
	breaches map[string]signal.Breach,
	class fleet.ClassName,
	quota budget.Quota,
	total int,
) (map[string]budget.Decision, map[string]fleet.Action) {

	acct := budget.New(map[fleet.ClassName]budget.Quota{class: quota})
	decisions := map[string]budget.Decision{}
	actions := map[string]fleet.Action{}

	order := make([]string, 0, len(assessments))
	for n := range assessments {
		order = append(order, n)
	}
	sort.Strings(order)

	for _, n := range order {
		a := assessments[n]
		act := actionFromString(a.RecommendedAction)
		actions[n] = act
		if act == fleet.ActionNone {
			continue
		}
		intent := fleet.Intent{
			Node: fleet.NodeID(n), Class: class, Action: act, Origin: fleet.OriginHealth,
			Signal: a.FaultClass, Severity: severityOf(breaches[n]), Observed: a.RootCause,
			RequestedAt: time.Now(),
		}
		d := acct.Admit(intent, fleet.FleetView{Class: class, Total: total, Unavailable: 0})
		if act.Disruptive() {
			decisions[n] = d
		}
	}
	return decisions, actions
}

func actionFromString(s string) fleet.Action {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "taint":
		return fleet.ActionTaint
	case "cordon":
		return fleet.ActionCordon
	case "drain":
		return fleet.ActionDrain
	case "quarantine":
		return fleet.ActionQuarantine
	default:
		return fleet.ActionNone
	}
}

func severityOf(b signal.Breach) fleet.Severity {
	if b.Severity == "" {
		return fleet.SeverityWarning
	}
	return b.Severity
}
