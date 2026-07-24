// Package controllers contains the reconcilers.
//
// Reconcilers do orchestration and nothing else: fetch state, delegate to the
// pure logic in internal/, apply the result. All decision-making lives in
// internal/, which is why the state machine, budget accountant and signal
// evaluator can be exhaustively unit tested without an API server.
package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	fleetv1alpha1 "github.com/melbadry97/fleetwarden/api/v1alpha1"
	"github.com/melbadry97/fleetwarden/internal/audit"
	"github.com/melbadry97/fleetwarden/internal/budget"
	"github.com/melbadry97/fleetwarden/internal/fleet"
	"github.com/melbadry97/fleetwarden/internal/lifecycle"
	"github.com/melbadry97/fleetwarden/internal/policy"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

const (
	// StateAnnotation records a node's lifecycle state. An annotation rather
	// than a label because state is not a selector target and we do not want
	// it churning label indexes on every transition.
	StateAnnotation = "fleet.elbadry.dev/state"
	// EnteredAnnotation records when the node entered its current state,
	// which is what the state machine's dwell times are measured against.
	EnteredAnnotation = "fleet.elbadry.dev/entered-state"
	// ClassLabel binds a node to its NodeClass.
	ClassLabel = "fleet.elbadry.dev/class"
	// ClearAnnotation is set by an operator to release a quarantine.
	ClearAnnotation = "fleet.elbadry.dev/clear-quarantine"

	// TaintKey is applied for non-disruptive flagging.
	TaintKey = "fleet.elbadry.dev/suspect"
)

// NodeHealthReconciler reconciles NodeHealthPolicy objects.
type NodeHealthReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Accountant is shared across all reconcilers so budget is genuinely
	// fleet-wide rather than per-policy.
	Accountant *budget.Accountant
	Audit      *audit.Log
	Evaluator  *signal.Evaluator
	Policy     *policy.Evaluator

	// Sources maps a source type to its implementation.
	Sources map[signal.Kind]signal.Source

	// Now is injectable for tests.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=fleet.elbadry.dev,resources=nodehealthpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleet.elbadry.dev,resources=nodehealthpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=fleet.elbadry.dev,resources=nodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=fleet.elbadry.dev,resources=nodeclasses/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile evaluates one policy against its class.
func (r *NodeHealthReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)
	now := r.now()

	var pol fleetv1alpha1.NodeHealthPolicy
	if err := r.Get(ctx, req.NamespacedName, &pol); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var class fleetv1alpha1.NodeClass
	if err := r.Get(ctx, client.ObjectKey{Name: pol.Spec.NodeClassRef.Name}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			l.Info("policy references a missing NodeClass", "class", pol.Spec.NodeClassRef.Name)
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, err
	}

	nodes, err := r.listClassNodes(ctx, &class)
	if err != nil {
		return ctrl.Result{}, err
	}

	snapshots, view := r.buildView(nodes, fleet.ClassName(class.Name))

	// 1. Collect breaches across every signal in the policy.
	var breaches []signal.Breach
	for _, s := range pol.Spec.Signals {
		spec, err := toSignalSpec(s)
		if err != nil {
			l.Error(err, "skipping malformed signal", "signal", s.Name)
			continue
		}
		src, ok := r.Sources[spec.Kind]
		if !ok {
			l.Info("no source registered for kind", "kind", spec.Kind)
			continue
		}
		samples, err := src.Fetch(spec)
		if err != nil {
			l.Error(err, "signal fetch failed", "signal", spec.Name)
			continue
		}
		got, err := r.Evaluator.Observe(spec, samples, now)
		if err != nil {
			l.Error(err, "signal evaluation failed", "signal", spec.Name)
			continue
		}
		breaches = append(breaches, got...)
	}

	// 2. Collapse breaches into at most one intent per node.
	intents := r.Policy.Evaluate(snapshots, breaches, now)
	byNode := make(map[fleet.NodeID]fleet.Intent, len(intents))
	for _, in := range intents {
		byNode[in.Node] = in
	}

	// 3. Run the state machine and apply admitted transitions.
	machine := lifecycle.NewMachine(escalationConfig(pol.Spec.Escalation))
	for id, snap := range snapshots {
		var intent *fleet.Intent
		if in, ok := byNode[id]; ok {
			intent = &in
		}

		node := nodes[id]
		drain := r.drainStatus(ctx, node)
		cleared := node.Annotations[ClearAnnotation] == "true"

		tr, moved := machine.Next(snap, intent, drain, cleared, now)
		if !moved {
			continue
		}

		if tr.RequiresBudget {
			// Intent may be nil only on paths that do not require budget, so
			// this dereference is safe; guard anyway rather than panic in a
			// reconcile loop.
			if intent == nil {
				continue
			}
			decision := r.Accountant.Admit(*intent, view)
			r.Audit.Append(audit.Record{
				Node: id, Class: fleet.ClassName(class.Name), Action: intent.Action,
				Origin: intent.Origin, Signal: intent.Signal, Observed: intent.Observed,
				Decision: decision.Reason, BudgetBefore: decision.BudgetBefore,
				BudgetAfter: decision.BudgetAfter,
				Outcome:     map[bool]audit.Outcome{true: audit.OutcomeAdmitted, false: audit.OutcomeDenied}[decision.Allowed],
			})
			if !decision.Allowed {
				r.Recorder.Eventf(&pol, corev1.EventTypeWarning, "BudgetExhausted",
					"denied %s on %s: %s", intent.Action, id, decision.Reason)
				continue
			}
			view.Unavailable++
		}

		if err := r.applyTransition(ctx, node, tr, now); err != nil {
			l.Error(err, "failed to apply transition", "node", id, "to", tr.To)
			r.Audit.Append(audit.Record{
				Node: id, Class: fleet.ClassName(class.Name),
				Decision: tr.Reason, Outcome: audit.OutcomeFailed,
			})
			continue
		}

		if tr.ReleasesBudget {
			r.Accountant.Release(fleet.ClassName(class.Name), id)
		}

		r.Recorder.Eventf(&pol, corev1.EventTypeNormal, "StateTransition",
			"%s: %s -> %s (%s)", id, tr.From, tr.To, tr.Reason)
	}

	// 4. Publish status.
	pol.Status.ObservedGeneration = pol.Generation
	pol.Status.NodesEvaluated = len(snapshots)
	pol.Status.ActiveBreaches = len(breaches)
	if err := r.Status().Update(ctx, &pol); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}

	class.Status.Total = view.Total
	class.Status.Unavailable = view.Unavailable
	class.Status.Healthy = view.Healthy()
	class.Status.BudgetConsumed = fmt.Sprintf("%d/%d",
		r.Accountant.InFlight(fleet.ClassName(class.Name)), view.Total)
	if err := r.Status().Update(ctx, &class); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}

	// Requeue on a fixed cadence: signals are polled, not watched, so
	// reconciliation must be time-driven rather than purely event-driven.
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *NodeHealthReconciler) listClassNodes(
	ctx context.Context, class *fleetv1alpha1.NodeClass,
) (map[fleet.NodeID]*corev1.Node, error) {

	sel, err := metav1.LabelSelectorAsSelector(class.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector on NodeClass %s: %w", class.Name, err)
	}
	var list corev1.NodeList
	if err := r.List(ctx, &list, &client.ListOptions{LabelSelector: sel}); err != nil {
		return nil, err
	}
	out := make(map[fleet.NodeID]*corev1.Node, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		out[fleet.NodeID(n.Name)] = n
	}
	return out, nil
}

func (r *NodeHealthReconciler) buildView(
	nodes map[fleet.NodeID]*corev1.Node, class fleet.ClassName,
) (map[fleet.NodeID]fleet.NodeSnapshot, fleet.FleetView) {

	snaps := make(map[fleet.NodeID]fleet.NodeSnapshot, len(nodes))
	view := fleet.FleetView{Class: class, Total: len(nodes)}

	for id, n := range nodes {
		state := fleet.State(n.Annotations[StateAnnotation])
		if !state.Valid() {
			state = fleet.StateHealthy
		}
		entered := r.now()
		if raw, ok := n.Annotations[EnteredAnnotation]; ok {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				entered = ts
			}
		}
		snaps[id] = fleet.NodeSnapshot{
			ID: id, Class: class, State: state,
			Schedulable:  !n.Spec.Unschedulable,
			EnteredState: entered,
			Ready:        nodeReady(n),
		}
		if state != fleet.StateHealthy && state != fleet.StateSuspect {
			view.Unavailable++
		}
	}
	return snaps, view
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// drainStatus reports whether an in-flight drain has finished, by counting
// pods that are neither mirror pods nor DaemonSet-owned.
func (r *NodeHealthReconciler) drainStatus(ctx context.Context, node *corev1.Node) lifecycle.DrainStatus {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return lifecycle.DrainStatus{}
	}
	remaining := 0
	for i := range pods.Items {
		if isDrainExempt(&pods.Items[i]) {
			continue
		}
		remaining++
	}
	return lifecycle.DrainStatus{InProgress: remaining > 0, Complete: remaining == 0}
}

// isDrainExempt reports whether a pod should be ignored when deciding whether
// a drain is complete. DaemonSet pods are never evicted and static pods cannot
// be, so counting them would mean no drain ever completes.
func isDrainExempt(p *corev1.Pod) bool {
	for _, or := range p.OwnerReferences {
		if or.Kind == "DaemonSet" {
			return true
		}
	}
	if _, mirror := p.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return true
	}
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// applyTransition writes the new state and performs the associated action.
func (r *NodeHealthReconciler) applyTransition(
	ctx context.Context, node *corev1.Node, tr lifecycle.Transition, now time.Time,
) error {
	patch := client.MergeFrom(node.DeepCopy())

	if node.Annotations == nil {
		node.Annotations = map[string]string{}
	}
	node.Annotations[StateAnnotation] = string(tr.To)
	node.Annotations[EnteredAnnotation] = now.Format(time.RFC3339)

	switch tr.To {
	case fleet.StateHealthy:
		node.Spec.Unschedulable = false
		removeTaint(node, TaintKey)
		delete(node.Annotations, ClearAnnotation)
	case fleet.StateSuspect:
		addTaint(node, corev1.Taint{
			Key: TaintKey, Value: "true", Effect: corev1.TaintEffectPreferNoSchedule,
		})
	case fleet.StateCordoned, fleet.StateDraining, fleet.StateQuarantined:
		node.Spec.Unschedulable = true
		addTaint(node, corev1.Taint{
			Key: TaintKey, Value: string(tr.To), Effect: corev1.TaintEffectNoSchedule,
		})
	}

	if err := r.Patch(ctx, node, patch); err != nil {
		return err
	}
	if tr.To == fleet.StateDraining {
		return r.evictPods(ctx, node)
	}
	return nil
}

// evictPods issues eviction requests, which respects PodDisruptionBudgets. We
// deliberately do not delete pods directly: bypassing PDBs here would defeat
// the point of having a disruption budget at all.
func (r *NodeHealthReconciler) evictPods(ctx context.Context, node *corev1.Node) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingFields{"spec.nodeName": node.Name}); err != nil {
		return err
	}
	var errs []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if isDrainExempt(p) {
			continue
		}
		ev := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace},
		}
		if err := r.SubResource("eviction").Create(ctx, p, ev); err != nil {
			// A PDB-blocked eviction is expected, not exceptional: the drain
			// timeout will force progress if it persists.
			if !apierrors.IsTooManyRequests(err) && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Sprintf("%s/%s: %v", p.Namespace, p.Name, err))
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("eviction errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

func addTaint(n *corev1.Node, t corev1.Taint) {
	for i := range n.Spec.Taints {
		if n.Spec.Taints[i].Key == t.Key {
			n.Spec.Taints[i] = t
			return
		}
	}
	n.Spec.Taints = append(n.Spec.Taints, t)
}

func removeTaint(n *corev1.Node, key string) {
	out := n.Spec.Taints[:0]
	for _, t := range n.Spec.Taints {
		if t.Key != key {
			out = append(out, t)
		}
	}
	n.Spec.Taints = out
}

func escalationConfig(e *fleetv1alpha1.EscalationSpec) lifecycle.Config {
	cfg := lifecycle.DefaultConfig()
	if e == nil {
		return cfg
	}
	if e.GracePeriod != nil {
		cfg.GracePeriod = e.GracePeriod.Duration
	}
	if e.DrainTimeout != nil {
		cfg.DrainTimeout = e.DrainTimeout.Duration
	}
	return cfg
}

// toSignalSpec converts the CRD shape into the internal, k8s-free spec.
func toSignalSpec(s fleetv1alpha1.SignalSpec) (signal.Spec, error) {
	spec := signal.Spec{
		Name:               s.Name,
		Kind:               signal.Kind(s.Source.Type),
		Key:                s.Source.Key,
		Query:              s.Source.Query,
		Operator:           signal.Operator(s.Operator),
		Threshold:          s.Threshold,
		RequireConsecutive: s.RequireConsecutive,
		Severity:           fleet.Severity(s.Severity),
		Action:             fleet.Action(s.Action),
	}
	if s.Window != nil {
		spec.Window = s.Window.Duration
	}
	return spec, spec.Validate()
}

// ParsePercent converts "5%" into 0.05. Exported for the webhook and manager.
func ParsePercent(s string) (float64, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(s), "%")
	v, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid percentage %q: %w", s, err)
	}
	if v < 0 || v > 100 {
		return 0, fmt.Errorf("percentage %q out of range", s)
	}
	return v / 100, nil
}

func (r *NodeHealthReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the reconciler and the pod-by-node index that
// drainStatus and evictPods rely on.
func (r *NodeHealthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &corev1.Pod{}, "spec.nodeName",
		func(o client.Object) []string {
			p, ok := o.(*corev1.Pod)
			if !ok || p.Spec.NodeName == "" {
				return nil
			}
			return []string{p.Spec.NodeName}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&fleetv1alpha1.NodeHealthPolicy{}).
		Owns(&fleetv1alpha1.NodeClass{}).
		Complete(r)
}
