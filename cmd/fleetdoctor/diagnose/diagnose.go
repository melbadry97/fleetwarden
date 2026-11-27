// Package diagnose turns a node's telemetry into a structured assessment: a
// fault classification, a prognosis, and a recommended action. Two
// implementations share one interface — a rule-based Offline diagnoser (no key
// required) and a Claude diagnoser — so the rest of the pipeline never cares
// which produced a verdict.
package diagnose

import (
	"strings"

	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/collect"
	"github.com/melbadry97/fleetwarden/cmd/fleetdoctor/detect"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

// NodeCase is everything known about one node, handed to a Diagnoser.
type NodeCase struct {
	Node     string                 `json:"node"`
	Series   []collect.Snapshot     `json:"series"`
	Breach   *signal.Breach         `json:"breach,omitempty"`
	Trend    *detect.Trend          `json:"trend,omitempty"`
	Firmware *collect.FirmwareProbe `json:"firmware,omitempty"`
}

// Prognosis is the forward-looking half of an assessment.
type Prognosis struct {
	FailureMode    string  `json:"failureMode"`
	EtaMinutes     float64 `json:"etaMinutes"`
	LeadTimeAction string  `json:"leadTimeAction"`
	Confidence     float64 `json:"confidence"`
}

// Assessment is the diagnoser's structured verdict for one node. Identical in
// shape whether produced by Claude or the offline rules.
type Assessment struct {
	Node              string    `json:"node"`
	FaultClass        string    `json:"faultClass"`
	RootCause         string    `json:"rootCause"`
	IsBmcLying        bool      `json:"isBmcLying"`
	Confidence        float64   `json:"confidence"`
	Reasoning         string    `json:"reasoning"`
	RecommendedAction string    `json:"recommendedAction"`
	Prognosis         Prognosis `json:"prognosis"`
	Source            string    `json:"source"`
}

// Diagnoser turns a NodeCase into an Assessment.
type Diagnoser interface {
	Assess(c NodeCase) (Assessment, error)
	Name() string
}

// Offline is a deterministic, rule-based diagnoser. It requires no API key and
// is the automatic fallback, so the pipeline always runs.
type Offline struct{}

// Name implements Diagnoser.
func (Offline) Name() string { return "offline" }

// Assess implements Diagnoser with a fixed decision tree. It prefers the least
// disruptive action that fits and never recommends draining for a firmware or
// credential fault.
func (Offline) Assess(c NodeCase) (Assessment, error) {
	a := Assessment{Node: c.Node, Source: "offline", Confidence: 0.9,
		RecommendedAction: "none", FaultClass: "healthy", RootCause: "no fault detected",
		Prognosis: Prognosis{EtaMinutes: -1, FailureMode: "none"}}

	if len(c.Series) == 0 {
		return a, nil
	}
	last := c.Series[len(c.Series)-1]

	switch {
	case !last.Reachable:
		a.FaultClass, a.RecommendedAction, a.Confidence = "unreachable", "cordon", 0.8
		a.RootCause = "BMC unreachable across polls"
		a.Reasoning = "No inventory response; cannot confirm node health out of band. Cordon pending an OOB session."
		return a, nil
	case !last.AuthOK:
		a.FaultClass, a.RecommendedAction, a.Confidence = "auth_fail", "none", 0.8
		a.RootCause = "BMC authenticated then refused the session"
		a.Reasoning = "Credential-broker issue, not a hardware fault. Do not disrupt the node."
		return a, nil
	}

	if fp := c.Firmware; fp != nil && fp.Accepted && !fp.Landed {
		a.FaultClass, a.IsBmcLying, a.RecommendedAction, a.Confidence = "firmware_ambiguous", true, "taint", 0.6
		a.RootCause = "firmware update accepted (202) but inventory still reports " + fp.Reported
		a.Reasoning = "Acceptance is not application. Either update-hangs (task runs forever) or stale-inventory (flash applied, inventory lies) — indistinguishable from inventory alone. Taint and reconcile task state against an independent inventory read; do not drain for a firmware issue."
		return a, nil
	}

	if b := c.Breach; b != nil {
		a.RecommendedAction = strings.ToLower(string(b.Action))
		switch b.Signal {
		case "eccUncorrectable":
			a.FaultClass, a.RootCause = "ecc_uncorrectable", "climbing uncorrectable ECC — HBM degradation"
			a.Reasoning = "Uncorrectable ECC at " + b.Observed + " and rising across consecutive polls; a windowed evaluator sees a rate, not a blip. Consistent with a dying card."
			if c.Trend != nil && c.Trend.EtaMinutes >= 0 {
				a.Prognosis = Prognosis{
					FailureMode:    "uncorrectable ECC crosses critical threshold",
					EtaMinutes:     c.Trend.EtaMinutes,
					LeadTimeAction: "pre-emptively cordon before the threshold rather than waiting for hard failure",
					Confidence:     0.7,
				}
			}
		case "pcieLinkDegraded":
			a.FaultClass, a.RootCause = "pcie_degraded", "PCIe link renegotiated to a narrower width"
			a.Reasoning = "Link width degraded and holding across polls. Throughput loss without a hard error; cordon and schedule reseat/repair."
		case "thermalThrottle":
			a.FaultClass, a.RootCause = "thermal_throttle", "sustained thermal throttling"
			a.Reasoning = "Throttle condition at " + b.Observed + " sustained across polls. Taint is non-disruptive and keeps the node in service while flagged."
		}
		return a, nil
	}

	if last.Health == "Critical" || last.Health == "Warning" {
		a.FaultClass, a.RecommendedAction, a.Confidence = "unknown", "taint", 0.5
		a.RootCause = "degraded health with no windowed breach"
		a.Reasoning = "Health is " + last.Health + " but no signal persisted past hysteresis. Taint and keep watching."
	}
	return a, nil
}
