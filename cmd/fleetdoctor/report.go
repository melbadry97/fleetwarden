package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/diagnose"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
)

// useColor is false when NO_COLOR is set or the terminal is dumb.
var useColor = os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"

func sgr(code, s string) string {
	if !useColor {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}
func dim(s string) string  { return sgr("2", s) }
func bold(s string) string { return sgr("1", s) }
func red(s string) string  { return sgr("31", s) }
func grn(s string) string  { return sgr("32", s) }
func yel(s string) string  { return sgr("33", s) }
func mag(s string) string  { return sgr("35", s) }

type reportInput struct {
	rack        string
	count       int
	polls       int
	diagnoser   string
	maxUnavail  float64
	minHealthy  int
	series      map[string][]collect.Snapshot
	flagged     []string
	assessments map[string]diagnose.Assessment
	decisions   map[string]budget.Decision
	actions     map[string]fleet.Action
}

func report(in reportInput) {
	ceil := int(float64(in.count) * in.maxUnavail)
	fmt.Printf("\n%s %s\n", bold("fleetdoctor"), dim("— diagnose + predict · out-of-band pipeline"))
	fmt.Printf("%s\n", dim(fmt.Sprintf("rack %s · %d nodes · %d polls · diagnoser=%s · ceiling %d (%.0f%%) floor %d",
		in.rack, in.count, in.polls, in.diagnoser, ceil, in.maxUnavail*100, in.minHealthy)))

	admitted, denied := 0, 0
	for _, n := range in.flagged {
		a := in.assessments[n]
		act := in.actions[n]
		last := in.series[n][len(in.series[n])-1]

		fmt.Printf("\n%s %s   %s\n", dot(a.FaultClass), bold(n), dim("health="+orNA(last.Health)))
		fmt.Printf("   %s  %s %s   %s %s\n", label("diagnosis"),
			classColor(a.FaultClass), dim(fmt.Sprintf("(conf %.2f)", a.Confidence)),
			dim("action→"), actionColor(act))
		if a.IsBmcLying {
			fmt.Printf("   %s  %s\n", label(""), red("⚠ BMC is lying — acceptance ≠ application"))
		}
		fmt.Printf("   %s  %s\n", label("root cause"), a.RootCause)
		fmt.Printf("   %s  %s\n", label("reasoning"), wrap(a.Reasoning, 74))
		if a.Prognosis.EtaMinutes >= 0 {
			fmt.Printf("   %s  %s%s\n", label("forecast"),
				yel(fmt.Sprintf("%s in ~%.1f min", a.Prognosis.FailureMode, a.Prognosis.EtaMinutes)),
				dim(fmt.Sprintf(" → %s (conf %.2f)", a.Prognosis.LeadTimeAction, a.Prognosis.Confidence)))
		}
		switch d, ok := in.decisions[n]; {
		case ok && d.Allowed:
			admitted++
			fmt.Printf("   %s  %s  %s\n", label("budget"), grn("ADMIT"), dim(d.Reason))
		case ok:
			denied++
			fmt.Printf("   %s  %s  %s\n", label("budget"), red("DENY "), dim(d.Reason))
		default:
			fmt.Printf("   %s  %s\n", label("budget"), dim("n/a — non-disruptive, no slot consumed"))
		}
	}

	healthy := in.count - len(in.flagged)
	fmt.Printf("\n%s\n", dim(strings.Repeat("─", 60)))
	fmt.Printf("%s  %s healthy · %s flagged   %s  %s admitted · %s denied\n",
		bold("summary"), grn(fmt.Sprint(healthy)), yel(fmt.Sprint(len(in.flagged))),
		dim("budget:"), grn(fmt.Sprint(admitted)), red(fmt.Sprint(denied)))
	fmt.Printf("%s\n\n", dim("the LLM diagnosed and forecast; the accountant decided what was allowed to disrupt."))
}

func dot(faultClass string) string { return colorFor(faultClass, "●") }

func classColor(c string) string { return colorFor(c, c) }

func colorFor(faultClass, s string) string {
	switch faultClass {
	case "ecc_uncorrectable", "unreachable", "auth_fail":
		return red(s)
	case "firmware_ambiguous", "firmware_stuck", "firmware_stale_inventory":
		return mag(s)
	case "healthy":
		return grn(s)
	default:
		return yel(s)
	}
}

func actionColor(a fleet.Action) string {
	s := strings.ToUpper(string(a))
	switch a {
	case fleet.ActionDrain, fleet.ActionQuarantine:
		return red(s)
	case fleet.ActionCordon:
		return sgr("38;5;208", s) // orange
	case fleet.ActionTaint:
		return yel(s)
	default:
		return dim(s)
	}
}

func label(s string) string { return dim(fmt.Sprintf("%-10s", s)) }

func orNA(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// wrap soft-wraps text to width, indenting continuation lines to align under
// the first column.
func wrap(s string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	const indent = "   " + "          " + "  " // matches the label column
	var b strings.Builder
	line := 0
	for _, w := range words {
		if line > 0 && line+1+len(w) > width {
			b.WriteString("\n" + indent)
			line = 0
		}
		if line > 0 {
			b.WriteString(" ")
			line++
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

func emitJSON(assessments map[string]diagnose.Assessment, decisions map[string]budget.Decision, actions map[string]fleet.Action) {
	type row struct {
		Assessment diagnose.Assessment `json:"assessment"`
		Action     string              `json:"decidedAction"`
		Budget     *budget.Decision    `json:"budget,omitempty"`
	}
	out := map[string]row{}
	for n, a := range assessments {
		r := row{Assessment: a, Action: string(actions[n])}
		if d, ok := decisions[n]; ok {
			dd := d
			r.Budget = &dd
		}
		out[n] = r
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
