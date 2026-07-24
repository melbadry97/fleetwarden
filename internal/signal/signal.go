// Package signal ingests raw telemetry and converts it into windowed,
// hysteresis-guarded breach determinations.
//
// The central design decision here is that signals are windowed rather than
// instantaneous. A single ECC error is noise; five in ten minutes is a dying
// card. Evaluating raw samples would produce a controller that flaps nodes in
// and out of service on transient blips.
package signal

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

// Kind identifies where a signal's samples come from.
type Kind string

const (
	// KindPrometheus evaluates a PromQL query.
	KindPrometheus Kind = "Prometheus"
	// KindNodeCondition reads a Kubernetes node condition.
	KindNodeCondition Kind = "NodeCondition"
	// KindNodeLabel reads a node label, used for firmware drift.
	KindNodeLabel Kind = "NodeLabel"
	// KindOutOfBand probes the BMC directly.
	KindOutOfBand Kind = "OutOfBand"
)

// Operator is the comparison applied to a sample value.
type Operator string

const (
	OpGreaterThan Operator = "GreaterThan"
	OpLessThan    Operator = "LessThan"
	OpEquals      Operator = "Equals"
	OpNotEquals   Operator = "NotEquals"
)

// Spec is one rule from a NodeHealthPolicy, decoupled from the CRD types so
// this package stays free of Kubernetes imports.
type Spec struct {
	Name      string
	Kind      Kind
	Key       string
	Query     string
	Operator  Operator
	Threshold string
	// Window is how long a breach must persist before it counts. Zero means
	// the signal fires on a single observation.
	Window time.Duration
	// RequireConsecutive adds hysteresis: this many consecutive breaching
	// observations are needed. Guards against a single scrape spike.
	RequireConsecutive int
	Severity           fleet.Severity
	Action             fleet.Action
}

// Validate checks a spec for internal consistency. Called by the validating
// webhook so unsafe policies are rejected at admission rather than discovered
// at reconcile time.
func (s Spec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("signal name is required")
	}
	switch s.Kind {
	case KindPrometheus, KindNodeCondition, KindNodeLabel, KindOutOfBand:
	default:
		return fmt.Errorf("signal %q: unknown kind %q", s.Name, s.Kind)
	}
	switch s.Operator {
	case OpGreaterThan, OpLessThan, OpEquals, OpNotEquals:
	default:
		return fmt.Errorf("signal %q: unknown operator %q", s.Name, s.Operator)
	}
	if s.Kind == KindPrometheus && strings.TrimSpace(s.Query) == "" {
		return fmt.Errorf("signal %q: prometheus source requires a query", s.Name)
	}
	if s.Kind != KindPrometheus && strings.TrimSpace(s.Key) == "" {
		return fmt.Errorf("signal %q: source requires a key", s.Name)
	}
	if s.Action == fleet.ActionNone {
		return fmt.Errorf("signal %q: action is required", s.Name)
	}
	if s.Window < 0 {
		return fmt.Errorf("signal %q: window must not be negative", s.Name)
	}
	if s.RequireConsecutive < 0 {
		return fmt.Errorf("signal %q: requireConsecutive must not be negative", s.Name)
	}
	// Numeric operators need a numeric threshold, otherwise every evaluation
	// silently fails at runtime.
	if s.Operator == OpGreaterThan || s.Operator == OpLessThan {
		if _, err := strconv.ParseFloat(s.Threshold, 64); err != nil {
			return fmt.Errorf("signal %q: operator %s requires a numeric threshold, got %q",
				s.Name, s.Operator, s.Threshold)
		}
	}
	return nil
}

// Sample is one observation of one signal on one node.
type Sample struct {
	Node  fleet.NodeID
	Value string
	At    time.Time
}

// Source fetches samples for a spec. Implementations live in the Kubernetes
// layer; the evaluator depends only on this interface.
type Source interface {
	Fetch(spec Spec) ([]Sample, error)
}

// breaches reports whether a value violates the spec.
func (s Spec) breaches(value string) (bool, error) {
	switch s.Operator {
	case OpEquals:
		return value == s.Threshold, nil
	case OpNotEquals:
		return value != s.Threshold, nil
	case OpGreaterThan, OpLessThan:
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return false, fmt.Errorf("signal %q: non-numeric sample %q: %w", s.Name, value, err)
		}
		t, err := strconv.ParseFloat(s.Threshold, 64)
		if err != nil {
			return false, fmt.Errorf("signal %q: non-numeric threshold %q: %w", s.Name, s.Threshold, err)
		}
		if s.Operator == OpGreaterThan {
			return v > t, nil
		}
		return v < t, nil
	}
	return false, fmt.Errorf("signal %q: unknown operator %q", s.Name, s.Operator)
}

// observation is a retained sample plus its breach verdict.
type observation struct {
	at     time.Time
	breach bool
	value  string
}

// key identifies a per-node, per-signal observation series.
type key struct {
	node   fleet.NodeID
	signal string
}

// Breach is the evaluator's verdict for one node and signal.
type Breach struct {
	Node     fleet.NodeID
	Signal   string
	Severity fleet.Severity
	Action   fleet.Action
	Observed string
	// Since is when the breach first began, used for dwell-time decisions.
	Since       time.Time
	Consecutive int
}

// Evaluator retains a bounded history per node and signal and decides whether
// a breach has persisted long enough to act on.
//
// It is not safe for concurrent use; the controller owns one per reconcile
// loop, which controller-runtime serialises per object.
type Evaluator struct {
	history map[key][]observation
	// retain bounds memory. Observations older than the longest configured
	// window plus this slack are discarded.
	retain time.Duration
	// maxPerSeries caps history length so a fast scrape interval on a large
	// fleet cannot exhaust memory.
	maxPerSeries int
}

// NewEvaluator builds an Evaluator. retain should exceed the longest window in
// any policy; 2x is a reasonable default.
func NewEvaluator(retain time.Duration) *Evaluator {
	if retain <= 0 {
		retain = time.Hour
	}
	return &Evaluator{
		history:      make(map[key][]observation),
		retain:       retain,
		maxPerSeries: 512,
	}
}

// Observe records samples for a spec and returns the breaches that have
// satisfied both the window and the consecutive-observation requirement.
//
// now is passed explicitly rather than read from the clock so tests are
// deterministic and reconciles are internally consistent.
func (e *Evaluator) Observe(spec Spec, samples []Sample, now time.Time) ([]Breach, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}

	// Deterministic ordering: callers get stable output regardless of the
	// source's iteration order, which matters for readable audit logs.
	sorted := make([]Sample, len(samples))
	copy(sorted, samples)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Node < sorted[j].Node })

	var out []Breach
	for _, s := range sorted {
		did, err := spec.breaches(s.Value)
		if err != nil {
			return nil, err
		}
		k := key{node: s.Node, signal: spec.Name}
		at := s.At
		if at.IsZero() {
			at = now
		}
		e.history[k] = append(e.history[k], observation{at: at, breach: did, value: s.Value})
		e.prune(k, now, spec.Window)

		if b, ok := e.verdict(k, spec, now); ok {
			out = append(out, b)
		}
	}
	return out, nil
}

// prune drops observations outside the retention horizon and enforces the
// per-series cap.
func (e *Evaluator) prune(k key, now time.Time, window time.Duration) {
	horizon := e.retain
	if window*2 > horizon {
		horizon = window * 2
	}
	cutoff := now.Add(-horizon)
	obs := e.history[k]
	i := 0
	for ; i < len(obs); i++ {
		if !obs[i].at.Before(cutoff) {
			break
		}
	}
	obs = obs[i:]
	if len(obs) > e.maxPerSeries {
		obs = obs[len(obs)-e.maxPerSeries:]
	}
	e.history[k] = obs
}

// verdict decides whether the retained series constitutes an actionable breach.
func (e *Evaluator) verdict(k key, spec Spec, now time.Time) (Breach, bool) {
	obs := e.history[k]
	if len(obs) == 0 {
		return Breach{}, false
	}
	last := obs[len(obs)-1]
	if !last.breach {
		return Breach{}, false
	}

	// Count the trailing run of consecutive breaches and find when it started.
	consecutive := 0
	since := last.at
	for i := len(obs) - 1; i >= 0; i-- {
		if !obs[i].breach {
			break
		}
		consecutive++
		since = obs[i].at
	}

	if spec.RequireConsecutive > 0 && consecutive < spec.RequireConsecutive {
		return Breach{}, false
	}
	// The breach must have persisted across the whole window. Using the run
	// start rather than the first-ever breach means a recovery resets the
	// clock, which is the behaviour operators expect.
	if spec.Window > 0 && now.Sub(since) < spec.Window {
		return Breach{}, false
	}

	return Breach{
		Node:        k.node,
		Signal:      spec.Name,
		Severity:    spec.Severity,
		Action:      spec.Action,
		Observed:    last.value,
		Since:       since,
		Consecutive: consecutive,
	}, true
}

// Forget drops all retained history for a node. Called when a node leaves the
// cluster so the evaluator does not leak memory across fleet churn.
func (e *Evaluator) Forget(node fleet.NodeID) {
	for k := range e.history {
		if k.node == node {
			delete(e.history, k)
		}
	}
}

// SeriesLen exposes retained observation count, for tests and metrics.
func (e *Evaluator) SeriesLen(node fleet.NodeID, signal string) int {
	return len(e.history[key{node: node, signal: signal}])
}
