// Package budget implements fleet-wide disruption accounting.
//
// Every disruptive action in fleetwarden — unplanned remediation, rollout
// waves, maintenance drains, pool downsizing — routes through Accountant.Admit.
// Modules do not disrupt nodes; they request disruption and receive a decision.
//
// The failure this prevents: a bad firmware push makes 200 nodes report
// unhealthy. A naive controller drains all 200 and the remediation becomes the
// outage. With central accounting the 11th drain is denied, the rollout halts,
// and a human is paged. Blast radius is bounded by construction rather than by
// hoping each subsystem is individually well behaved.
package budget

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/melbadry97/fleetwarden/internal/fleet"
)

// Quota is the disruption ceiling for one NodeClass.
type Quota struct {
	// MaxUnavailableRatio caps concurrently unavailable nodes as a fraction of
	// class size, e.g. 0.05 for 5%.
	MaxUnavailableRatio float64
	// MinHealthyAbsolute is a hard floor. The class never drops below this many
	// serving nodes regardless of what the ratio would permit. This exists
	// because a percentage of a small class rounds to something dangerous.
	MinHealthyAbsolute int
	// PlannedShare caps the fraction of the budget that scheduled work
	// (rollouts, maintenance, downsizing) may consume, reserving headroom for
	// unplanned hardware failure. Zero means planned work may use the whole
	// budget.
	PlannedShare float64
}

// DefaultQuota is used for classes with no explicit quota configured. It is
// deliberately conservative: an unconfigured class should not be drainable in
// bulk by accident.
func DefaultQuota() Quota {
	return Quota{MaxUnavailableRatio: 0.05, MinHealthyAbsolute: 1, PlannedShare: 0.6}
}

// Decision is the outcome of an admission request.
type Decision struct {
	Allowed bool
	// Reason is always populated, including on allow, so the audit record
	// explains why an action was permitted.
	Reason string
	// RetryAfter is a hint for denied intents that may succeed later.
	RetryAfter time.Duration
	// BudgetBefore and BudgetAfter are unavailable counts, captured for audit.
	BudgetBefore int
	BudgetAfter  int
}

func allow(reason string, before, after int) Decision {
	return Decision{Allowed: true, Reason: reason, BudgetBefore: before, BudgetAfter: after}
}

func deny(reason string, retry time.Duration, before int) Decision {
	return Decision{Allowed: false, Reason: reason, RetryAfter: retry, BudgetBefore: before, BudgetAfter: before}
}

// entry is one in-flight disruption held on the ledger.
type entry struct {
	origin  fleet.Origin
	action  fleet.Action
	startAt time.Time
}

// Clock is injected so tests control time without sleeping.
type Clock func() time.Time

// Accountant is the single admission point for disruptive intents.
//
// It is safe for concurrent use: controller-runtime may reconcile multiple
// objects in parallel and a racy budget is the same as no budget.
type Accountant struct {
	mu     sync.Mutex
	quotas map[fleet.ClassName]Quota
	// ledger tracks in-flight disruptions keyed by class then node.
	ledger map[fleet.ClassName]map[fleet.NodeID]entry
	now    Clock
	// stale bounds how long an entry may sit on the ledger before it is
	// reclaimed. Without this, a controller crash mid-drain would leak budget
	// permanently and eventually wedge the fleet.
	stale time.Duration
}

// Option configures an Accountant.
type Option func(*Accountant)

// WithClock injects a deterministic clock for testing.
func WithClock(c Clock) Option { return func(a *Accountant) { a.now = c } }

// WithStaleTimeout sets how long in-flight entries survive without release.
func WithStaleTimeout(d time.Duration) Option { return func(a *Accountant) { a.stale = d } }

// New builds an Accountant with per-class quotas.
func New(quotas map[fleet.ClassName]Quota, opts ...Option) *Accountant {
	a := &Accountant{
		quotas: make(map[fleet.ClassName]Quota, len(quotas)),
		ledger: make(map[fleet.ClassName]map[fleet.NodeID]entry),
		now:    time.Now,
		stale:  30 * time.Minute,
	}
	for k, v := range quotas {
		a.quotas[k] = v
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Quota returns the configured quota for a class, or the default.
func (a *Accountant) Quota(c fleet.ClassName) Quota {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.quotaLocked(c)
}

func (a *Accountant) quotaLocked(c fleet.ClassName) Quota {
	if q, ok := a.quotas[c]; ok {
		return q
	}
	return DefaultQuota()
}

// Admit decides whether an intent may proceed against the given fleet view.
//
// Non-disruptive actions are always admitted. Disruptive actions must satisfy
// three independent constraints: the ratio ceiling, the absolute healthy floor,
// and — for planned work — the reserved planned share.
func (a *Accountant) Admit(intent fleet.Intent, view fleet.FleetView) Decision {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.reapLocked(intent.Class)

	if !intent.Action.Disruptive() {
		return allow("action is non-disruptive", view.Unavailable, view.Unavailable)
	}

	inFlight := a.ledger[intent.Class]

	// Idempotence: re-admitting a node already in flight must not double-count
	// budget. Reconcile loops are at-least-once, so this path is hit routinely
	// rather than exceptionally.
	if _, held := inFlight[intent.Node]; held {
		return allow("node already holds a budget slot", view.Unavailable, view.Unavailable)
	}

	q := a.quotaLocked(intent.Class)

	if view.Total <= 0 {
		return deny("class has no nodes", 0, view.Unavailable)
	}

	// The caller's view is a snapshot taken at the start of a reconcile. Under
	// parallel reconciles it is stale the moment a sibling goroutine is
	// admitted, and trusting it alone means N concurrent callers each see zero
	// unavailable and all get admitted — the budget silently does nothing.
	//
	// The ledger is authoritative for disruptions this controller initiated;
	// the view is authoritative for nodes down for reasons we did not cause.
	// Taking the maximum is correct for both: when the view is fresh it
	// already includes our in-flight work, and when it is stale the ledger
	// covers the gap. Summing would double-count the overlap and starve the
	// fleet of legitimate capacity.
	before := view.Unavailable
	if n := len(inFlight); n > before {
		before = n
	}
	after := before + 1

	// Constraint 1: absolute healthy floor. Checked before the ratio because
	// it is the constraint that protects small classes, where a percentage
	// ceiling is meaningless.
	if view.Total-after < q.MinHealthyAbsolute {
		return deny(fmt.Sprintf("would leave %d healthy, below floor of %d",
			view.Total-after, q.MinHealthyAbsolute), 5*time.Minute, before)
	}

	// Constraint 2: ratio ceiling. Floor the allowance so a 5% ceiling on 10
	// nodes permits zero, not one — rounding up here would silently double the
	// intended blast radius on small classes.
	maxUnavailable := int(math.Floor(float64(view.Total) * q.MaxUnavailableRatio))
	if after > maxUnavailable {
		return deny(fmt.Sprintf("would exceed maxUnavailable %d (%.0f%% of %d)",
			maxUnavailable, q.MaxUnavailableRatio*100, view.Total), 5*time.Minute, before)
	}

	// Constraint 3: planned work may not consume the headroom reserved for
	// unplanned failure. Unplanned work may use the entire budget, because a
	// dying node does not wait for a maintenance window.
	if intent.Origin.Planned() && q.PlannedShare > 0 {
		plannedCap := int(math.Floor(float64(maxUnavailable) * q.PlannedShare))
		plannedInFlight := 0
		for _, e := range inFlight {
			if e.origin.Planned() {
				plannedInFlight++
			}
		}
		if plannedInFlight+1 > plannedCap {
			return deny(fmt.Sprintf("planned share exhausted: %d in flight, cap %d (%.0f%% of %d)",
				plannedInFlight, plannedCap, q.PlannedShare*100, maxUnavailable),
				10*time.Minute, before)
		}
	}

	if inFlight == nil {
		inFlight = make(map[fleet.NodeID]entry)
		a.ledger[intent.Class] = inFlight
	}
	inFlight[intent.Node] = entry{origin: intent.Origin, action: intent.Action, startAt: a.now()}

	return allow(fmt.Sprintf("admitted within budget %d/%d", after, maxUnavailable), before, after)
}

// Release returns a node's budget slot once it is serving again or has reached
// a terminal state that no longer counts against availability.
func (a *Accountant) Release(class fleet.ClassName, node fleet.NodeID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m, ok := a.ledger[class]; ok {
		delete(m, node)
		if len(m) == 0 {
			delete(a.ledger, class)
		}
	}
}

// InFlight reports how many disruptions are currently held for a class.
func (a *Accountant) InFlight(class fleet.ClassName) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reapLocked(class)
	return len(a.ledger[class])
}

// Holds reports whether a specific node currently holds a budget slot.
func (a *Accountant) Holds(class fleet.ClassName, node fleet.NodeID) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.ledger[class][node]
	return ok
}

// reapLocked drops ledger entries older than the stale timeout. A controller
// that crashes mid-drain would otherwise leak its slot forever.
func (a *Accountant) reapLocked(class fleet.ClassName) {
	m, ok := a.ledger[class]
	if !ok {
		return
	}
	cutoff := a.now().Add(-a.stale)
	for id, e := range m {
		if e.startAt.Before(cutoff) {
			delete(m, id)
		}
	}
	if len(m) == 0 {
		delete(a.ledger, class)
	}
}
