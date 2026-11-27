// Package detect turns a Redfish telemetry series into breach determinations
// and a deterministic forecast. Breach detection reuses internal/signal, so the
// windowing and hysteresis are the exact same code the controller runs; the
// Redfish->signal adapter here is the oobSource the design doc left stubbed.
package detect

import (
	"strconv"
	"time"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

// signalSpec pairs a signal.Spec with a function that extracts its value from a
// snapshot.
type signalSpec struct {
	spec  signal.Spec
	value func(collect.Snapshot) string
}

func condOr(name, fallback string) func(collect.Snapshot) string {
	return func(s collect.Snapshot) string {
		if v, ok := s.Condition(name); ok {
			return v
		}
		return fallback
	}
}

// specs are the out-of-band signals the pipeline watches. Windows are zero
// (the simulator compresses time); hysteresis via RequireConsecutive is what
// prevents a single poll's spike from counting.
func specs() []signalSpec {
	return []signalSpec{
		{
			spec: signal.Spec{
				Name: "eccUncorrectable", Kind: signal.KindOutOfBand, Key: "ECCUncorrectable",
				Operator: signal.OpGreaterThan, Threshold: "0", RequireConsecutive: 2,
				Severity: fleet.SeverityCritical, Action: fleet.ActionDrain,
			},
			value: condOr("ECCUncorrectable", "0"),
		},
		{
			spec: signal.Spec{
				Name: "pcieLinkDegraded", Kind: signal.KindOutOfBand, Key: "PCIeLinkWidthDegraded",
				Operator: signal.OpEquals, Threshold: "True", RequireConsecutive: 2,
				Severity: fleet.SeverityWarning, Action: fleet.ActionCordon,
			},
			value: condOr("PCIeLinkWidthDegraded", "False"),
		},
		{
			spec: signal.Spec{
				Name: "thermalThrottle", Kind: signal.KindOutOfBand, Key: "ThermalThrottle",
				Operator: signal.OpGreaterThan, Threshold: "40", RequireConsecutive: 2,
				Severity: fleet.SeverityWarning, Action: fleet.ActionTaint,
			},
			value: condOr("ThermalThrottle", "0"),
		},
	}
}

// Detect runs every spec through a signal.Evaluator across all polls and
// returns, per node, the single most-urgent breach (highest severity, then
// highest action rank). Unreachable/unauthenticated polls are skipped so a
// collection failure never masquerades as a healthy sample.
func Detect(series map[string][]collect.Snapshot) map[string]signal.Breach {
	out := map[string]signal.Breach{}
	polls := 0
	for _, snaps := range series {
		if len(snaps) > polls {
			polls = len(snaps)
		}
	}

	for _, ss := range specs() {
		ev := signal.NewEvaluator(time.Hour)
		for p := 0; p < polls; p++ {
			var samples []signal.Sample
			var now time.Time
			for node, snaps := range series {
				if p >= len(snaps) {
					continue
				}
				snap := snaps[p]
				if !snap.Reachable || !snap.AuthOK {
					continue
				}
				now = snap.At
				samples = append(samples, signal.Sample{
					Node: fleet.NodeID(node), Value: ss.value(snap), At: snap.At,
				})
			}
			breaches, err := ev.Observe(ss.spec, samples, now)
			if err != nil {
				continue
			}
			for _, b := range breaches {
				node := string(b.Node)
				if cur, ok := out[node]; !ok || moreUrgent(b, cur) {
					out[node] = b
				}
			}
		}
	}
	return out
}

func moreUrgent(a, b signal.Breach) bool {
	if a.Severity.Rank() != b.Severity.Rank() {
		return a.Severity.Rank() > b.Severity.Rank()
	}
	return a.Action.Rank() > b.Action.Rank()
}

// Trend is the deterministic forecast anchor. The diagnoser narrates a
// prognosis, but the numbers come from here, so the forecast has an auditable
// basis rather than a hallucinated one.
type Trend struct {
	Signal       string  `json:"signal"`
	First        float64 `json:"first"`
	Last         float64 `json:"last"`
	SlopePerPoll float64 `json:"slopePerPoll"`
	Threshold    float64 `json:"threshold"`
	EtaPolls     float64 `json:"etaPolls"`
	EtaMinutes   float64 `json:"etaMinutes"` // -1 when not projectable
	Rising       bool    `json:"rising"`
}

// ECCTrend fits a per-poll slope to a node's ECC series and projects time to a
// critical count, expressed in minutes at the assumed production poll cadence
// (the simulator polls in milliseconds; the cadence makes the ETA realistic).
func ECCTrend(snaps []collect.Snapshot, critical float64, cadence time.Duration) (Trend, bool) {
	var vals []float64
	for _, s := range snaps {
		if v, ok := s.Condition("ECCUncorrectable"); ok {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				vals = append(vals, f)
			}
		}
	}
	if len(vals) < 2 {
		return Trend{}, false
	}
	first, last := vals[0], vals[len(vals)-1]
	slope := (last - first) / float64(len(vals)-1)
	t := Trend{
		Signal: "eccUncorrectable", First: first, Last: last,
		SlopePerPoll: slope, Threshold: critical, EtaMinutes: -1, Rising: slope > 0,
	}
	if slope > 0 && last < critical {
		t.EtaPolls = (critical - last) / slope
		t.EtaMinutes = t.EtaPolls * cadence.Minutes()
	}
	return t, true
}
