// Command fleetdoctor is an AI-assisted diagnose-and-predict pipeline over the
// fakeredfish simulator. It polls out-of-band BMC telemetry, detects breaches
// with the same windowing/hysteresis the controller uses, asks Claude to
// diagnose the fault and forecast its progression, and routes the
// recommendation through the real disruption-budget accountant.
//
// The deliberate shape: the LLM advises, the deterministic accountant decides.
// A probabilistic component never drains a node directly — it emits an intent
// and the budget admits or denies it. Diagnosis can be creative; disruption
// cannot.
//
// Pipeline: collect -> detect -> diagnose -> predict -> decide, one package per
// stage, each independently tested.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/detect"
	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/diagnose"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
)

type config struct {
	addr        string
	rack        string
	count       int
	polls       int
	interval    time.Duration
	firmware    string
	offline     bool
	model       string
	eccCritical float64
	cadence     time.Duration
	maxUnavail  float64
	minHealthy  int
	asJSON      bool
}

func main() {
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "fleetdoctor:", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var c config
	flag.StringVar(&c.addr, "addr", "localhost:8443", "fakeredfish address")
	flag.StringVar(&c.rack, "rack", "r14", "rack / node class name")
	flag.IntVar(&c.count, "count", 16, "number of nodes in the rack")
	flag.IntVar(&c.polls, "polls", 4, "telemetry polls per node")
	flag.DurationVar(&c.interval, "interval", 700*time.Millisecond, "delay between polls")
	flag.StringVar(&c.firmware, "firmware", "", "if set, probe a firmware update to this version and diagnose the result")
	flag.BoolVar(&c.offline, "offline", false, "use the rule-based diagnoser instead of calling Claude")
	flag.StringVar(&c.model, "model", "claude-haiku-5-5", "Claude model for diagnosis")
	flag.Float64Var(&c.eccCritical, "ecc-critical", 200, "ECC count treated as card-failure threshold for the forecast")
	flag.DurationVar(&c.cadence, "poll-cadence", 30*time.Second, "assumed production poll cadence, used only to express the forecast ETA in minutes")
	flag.Float64Var(&c.maxUnavail, "max-unavailable", 0.25, "disruption ceiling as a fraction of the class")
	flag.IntVar(&c.minHealthy, "min-healthy", 0, "absolute healthy floor (0 = count/2)")
	flag.BoolVar(&c.asJSON, "json", false, "emit the full assessment as JSON")
	flag.Parse()
	if c.minHealthy == 0 {
		c.minHealthy = c.count / 2
	}
	return c
}

func run(cfg config) error {
	diag := pickDiagnoser(cfg)

	client := collect.New("http://" + cfg.addr)
	nodes := make([]string, cfg.count)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("%s-%d", cfg.rack, i)
	}

	// ---- collect ----
	series := map[string][]collect.Snapshot{}
	for p := 0; p < cfg.polls; p++ {
		for _, n := range nodes {
			series[n] = append(series[n], client.Poll(n, time.Now()))
		}
		if p < cfg.polls-1 {
			time.Sleep(cfg.interval)
		}
	}
	firmwareProbes := map[string]collect.FirmwareProbe{}
	if cfg.firmware != "" {
		for _, n := range nodes {
			firmwareProbes[n] = client.PushFirmware(n, cfg.firmware, 2500*time.Millisecond)
		}
	}

	// ---- detect ----
	breaches := detect.Detect(series)

	// ---- assess (diagnose + predict) ----
	assessments := map[string]diagnose.Assessment{}
	for _, n := range nodes {
		last := series[n][len(series[n])-1]
		b, hasBreach := breaches[n]
		fp, hasFW := firmwareProbes[n]
		fwAnomaly := hasFW && fp.Accepted && !fp.Landed
		interesting := hasBreach || fwAnomaly || !last.Reachable || !last.AuthOK || (last.Health != "OK" && last.Health != "")
		if !interesting {
			continue
		}
		c := diagnose.NodeCase{Node: n, Series: series[n]}
		if hasBreach {
			bb := b
			c.Breach = &bb
		}
		if hasFW {
			ff := fp
			c.Firmware = &ff
		}
		if t, ok := detect.ECCTrend(series[n], cfg.eccCritical, cfg.cadence); ok {
			c.Trend = &t
		}
		a, err := diag.Assess(c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "assess %s: %v\n", n, err)
			a = diagnose.Assessment{Node: n, FaultClass: "unknown", RecommendedAction: "none",
				Reasoning: "diagnoser error: " + err.Error(), Source: diag.Name()}
		}
		assessments[n] = a
	}

	// ---- decide: route recommendations through the real accountant ----
	class := fleet.ClassName(cfg.rack)
	quota := budget.Quota{MaxUnavailableRatio: cfg.maxUnavail, MinHealthyAbsolute: cfg.minHealthy}
	decisions, actions := decide(assessments, breaches, class, quota, cfg.count)

	// ---- report ----
	if cfg.asJSON {
		emitJSON(assessments, decisions, actions)
		return nil
	}
	flagged := make([]string, 0, len(assessments))
	for n := range assessments {
		flagged = append(flagged, n)
	}
	sort.Strings(flagged)
	report(reportInput{
		rack: cfg.rack, count: cfg.count, polls: cfg.polls, diagnoser: diag.Name(),
		maxUnavail: cfg.maxUnavail, minHealthy: cfg.minHealthy,
		series: series, flagged: flagged, assessments: assessments,
		decisions: decisions, actions: actions,
	})
	return nil
}

// pickDiagnoser chooses Claude when a key is present and offline is not forced,
// falling back to the rule engine with a note so the pipeline always runs.
func pickDiagnoser(cfg config) diagnose.Diagnoser {
	key := os.Getenv("ANTHROPIC_API_KEY")
	switch {
	case cfg.offline:
		return diagnose.Offline{}
	case key == "":
		fmt.Fprintln(os.Stderr, dim("ANTHROPIC_API_KEY not set — running the offline rule-based diagnoser. Set a key (and drop --offline) to use Claude."))
		return diagnose.Offline{}
	default:
		return diagnose.NewClaude(key, cfg.model)
	}
}
