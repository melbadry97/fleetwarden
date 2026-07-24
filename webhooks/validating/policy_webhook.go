// Package validating rejects unsafe objects at admission rather than
// discovering the problem at reconcile time.
//
// The distinction matters operationally: a policy with a non-numeric threshold
// under a numeric operator fails on every evaluation, silently, forever. The
// webhook turns that into an immediate, legible kubectl error.
package validating

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	fleetv1alpha1 "github.com/melbadry97/fleetwarden/api/v1alpha1"
	"github.com/melbadry97/fleetwarden/controllers"
	"github.com/melbadry97/fleetwarden/internal/signal"
)

// PolicyValidator validates NodeHealthPolicy objects.
type PolicyValidator struct{}

var _ webhook.CustomValidator = &PolicyValidator{}

// SetupWithManager registers the webhook.
func (v *PolicyValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1alpha1.NodeHealthPolicy{}).
		WithValidator(v).
		Complete()
}

// +kubebuilder:webhook:path=/validate-fleet-elbadry-dev-v1alpha1-nodehealthpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=fleet.elbadry.dev,resources=nodehealthpolicies,verbs=create;update,versions=v1alpha1,name=vnodehealthpolicy.kb.io,admissionReviewVersions=v1

// ValidateCreate implements webhook.CustomValidator.
func (v *PolicyValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj)
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *PolicyValidator) ValidateUpdate(_ context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj)
}

// ValidateDelete implements webhook.CustomValidator.
func (v *PolicyValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *PolicyValidator) validate(obj runtime.Object) (admission.Warnings, error) {
	pol, ok := obj.(*fleetv1alpha1.NodeHealthPolicy)
	if !ok {
		return nil, fmt.Errorf("expected NodeHealthPolicy, got %T", obj)
	}

	var errs field.ErrorList
	var warns admission.Warnings
	seen := map[string]bool{}

	for i, s := range pol.Spec.Signals {
		p := field.NewPath("spec", "signals").Index(i)

		if seen[s.Name] {
			errs = append(errs, field.Duplicate(p.Child("name"), s.Name))
		}
		seen[s.Name] = true

		spec := signal.Spec{
			Name:               s.Name,
			Kind:               signal.Kind(s.Source.Type),
			Key:                s.Source.Key,
			Query:              s.Source.Query,
			Operator:           signal.Operator(s.Operator),
			Threshold:          s.Threshold,
			RequireConsecutive: s.RequireConsecutive,
		}
		if s.Window != nil {
			spec.Window = s.Window.Duration
		}
		// Reuse the same validation the evaluator applies, so a policy that
		// passes admission is guaranteed to be evaluable.
		spec.Severity = "Warning"
		spec.Action = "Cordon"
		if err := spec.Validate(); err != nil {
			errs = append(errs, field.Invalid(p, s.Name, err.Error()))
		}

		// A window shorter than a typical scrape interval produces a signal
		// that fires on a single sample, which is flapping by construction.
		if s.Window != nil && s.Window.Duration > 0 && s.Window.Duration < 30_000_000_000 {
			warns = append(warns, fmt.Sprintf(
				"signal %q has a window under 30s, shorter than a typical scrape interval; expect flapping", s.Name))
		}

		// Quarantine is effectively terminal. Allowing it to clear itself
		// automatically would mask a recurring hardware fault.
		if s.Action == "Quarantine" && (pol.Spec.Repair == nil || !pol.Spec.Repair.RequireManualClear) {
			errs = append(errs, field.Invalid(p.Child("action"), s.Action,
				"Quarantine requires spec.repair.requireManualClear to be true"))
		}
	}

	if pol.Spec.Escalation != nil && pol.Spec.Escalation.DrainTimeout != nil {
		if pol.Spec.Escalation.DrainTimeout.Duration <= 0 {
			errs = append(errs, field.Invalid(
				field.NewPath("spec", "escalation", "drainTimeout"),
				pol.Spec.Escalation.DrainTimeout.Duration,
				"must be positive; without a timeout a blocked drain pins a budget slot indefinitely"))
		}
	}

	if len(errs) > 0 {
		return warns, errs.ToAggregate()
	}
	return warns, nil
}

// ClassValidator enforces disruption quota sanity on NodeClass.
type ClassValidator struct{}

var _ webhook.CustomValidator = &ClassValidator{}

// SetupWithManager registers the webhook.
func (v *ClassValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&fleetv1alpha1.NodeClass{}).
		WithValidator(v).
		Complete()
}

// +kubebuilder:webhook:path=/validate-fleet-elbadry-dev-v1alpha1-nodeclass,mutating=false,failurePolicy=fail,sideEffects=None,groups=fleet.elbadry.dev,resources=nodeclasses,verbs=create;update,versions=v1alpha1,name=vnodeclass.kb.io,admissionReviewVersions=v1

// ValidateCreate implements webhook.CustomValidator.
func (v *ClassValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj)
}

// ValidateUpdate implements webhook.CustomValidator.
func (v *ClassValidator) ValidateUpdate(_ context.Context, _, obj runtime.Object) (admission.Warnings, error) {
	return v.validate(obj)
}

// ValidateDelete implements webhook.CustomValidator.
func (v *ClassValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

func (v *ClassValidator) validate(obj runtime.Object) (admission.Warnings, error) {
	class, ok := obj.(*fleetv1alpha1.NodeClass)
	if !ok {
		return nil, fmt.Errorf("expected NodeClass, got %T", obj)
	}

	var errs field.ErrorList
	var warns admission.Warnings
	base := field.NewPath("spec", "disruption")

	ratio, err := controllers.ParsePercent(class.Spec.Disruption.MaxUnavailable)
	if err != nil {
		errs = append(errs, field.Invalid(base.Child("maxUnavailable"),
			class.Spec.Disruption.MaxUnavailable, err.Error()))
	} else if ratio > 0.25 {
		// Not an error — an operator may genuinely want this — but it is
		// almost always a mistake on a production accelerator class.
		warns = append(warns, fmt.Sprintf(
			"maxUnavailable of %s permits draining over a quarter of the class at once",
			class.Spec.Disruption.MaxUnavailable))
	}

	if class.Spec.Disruption.PlannedShare != "" {
		if _, err := controllers.ParsePercent(class.Spec.Disruption.PlannedShare); err != nil {
			errs = append(errs, field.Invalid(base.Child("plannedShare"),
				class.Spec.Disruption.PlannedShare, err.Error()))
		}
	}

	if class.Spec.Disruption.MinHealthyAbsolute < 0 {
		errs = append(errs, field.Invalid(base.Child("minHealthyAbsolute"),
			class.Spec.Disruption.MinHealthyAbsolute, "must not be negative"))
	}

	// A class with no floor and no meaningful ratio is fully drainable, which
	// is worth surfacing loudly.
	if class.Spec.Disruption.MinHealthyAbsolute == 0 && err == nil && ratio >= 1.0 {
		warns = append(warns, "this class has no availability floor and permits full drain")
	}

	if len(errs) > 0 {
		return warns, errs.ToAggregate()
	}
	return warns, nil
}
