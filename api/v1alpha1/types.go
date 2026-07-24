// Package v1alpha1 contains the fleetwarden API types.
//
// +kubebuilder:object:generate=true
// +groupName=fleet.elbadry.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version for these types.
	GroupVersion = schema.GroupVersion{Group: "fleet.elbadry.dev", Version: "v1alpha1"}

	// SchemeBuilder registers these types with a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds these types to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// ---------------------------------------------------------------------------
// NodeClass
// ---------------------------------------------------------------------------

// AcceleratorSpec describes the accelerators on a node.
type AcceleratorSpec struct {
	// +kubebuilder:validation:Minimum=0
	Count int `json:"count"`
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	Interconnect string `json:"interconnect,omitempty"`
}

// HardwareSpec describes the physical shape of a node in this class.
type HardwareSpec struct {
	// +optional
	Accelerators *AcceleratorSpec `json:"accelerators,omitempty"`
	// +optional
	Memory string `json:"memory,omitempty"`
	// +optional
	PowerEnvelopeWatts int `json:"powerEnvelopeWatts,omitempty"`
}

// DisruptionSpec is the class-scoped disruption quota enforced by the budget
// accountant.
type DisruptionSpec struct {
	// MaxUnavailable is the ceiling on concurrently unavailable nodes,
	// expressed as a percentage string such as "5%".
	// +kubebuilder:validation:Pattern=`^([0-9]|[1-9][0-9]|100)%$`
	// +kubebuilder:default="5%"
	MaxUnavailable string `json:"maxUnavailable"`

	// MinHealthyAbsolute is a hard floor on serving nodes. It exists because a
	// percentage of a small class rounds to something dangerous.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	MinHealthyAbsolute int `json:"minHealthyAbsolute"`

	// PlannedShare caps the fraction of the budget that scheduled work may
	// consume, reserving headroom for unplanned hardware failure.
	// +kubebuilder:validation:Pattern=`^([0-9]|[1-9][0-9]|100)%$`
	// +kubebuilder:default="60%"
	// +optional
	PlannedShare string `json:"plannedShare,omitempty"`
}

// FirmwareBaseline is the expected firmware set for the class. Drift from this
// baseline is what the firmwareDrift signal detects.
type FirmwareBaseline struct {
	// +optional
	BMC string `json:"bmc,omitempty"`
	// +optional
	BIOS string `json:"bios,omitempty"`
	// +optional
	CPLD string `json:"cpld,omitempty"`
}

// NodeClassSpec defines a hardware SKU grouping.
type NodeClassSpec struct {
	Selector *metav1.LabelSelector `json:"selector"`
	// +optional
	Hardware   *HardwareSpec  `json:"hardware,omitempty"`
	Disruption DisruptionSpec `json:"disruption"`
	// +optional
	Firmware *FirmwareBaseline `json:"firmware,omitempty"`
}

// NodeClassStatus reports observed class state.
type NodeClassStatus struct {
	// +optional
	Total int `json:"total"`
	// +optional
	Healthy int `json:"healthy"`
	// +optional
	Unavailable int `json:"unavailable"`
	// +optional
	BudgetConsumed string `json:"budgetConsumed,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nc
// +kubebuilder:printcolumn:name="Total",type=integer,JSONPath=`.status.total`
// +kubebuilder:printcolumn:name="Healthy",type=integer,JSONPath=`.status.healthy`
// +kubebuilder:printcolumn:name="Unavailable",type=integer,JSONPath=`.status.unavailable`
// +kubebuilder:printcolumn:name="Budget",type=string,JSONPath=`.status.budgetConsumed`

// NodeClass groups nodes by hardware SKU and carries the disruption quota.
type NodeClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeClassSpec   `json:"spec,omitempty"`
	Status NodeClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeClassList is a list of NodeClass.
type NodeClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeClass `json:"items"`
}

// ---------------------------------------------------------------------------
// NodeHealthPolicy
// ---------------------------------------------------------------------------

// SignalSource describes where a signal's samples come from.
type SignalSource struct {
	// +kubebuilder:validation:Enum=Prometheus;NodeCondition;NodeLabel;OutOfBand
	Type string `json:"type"`
	// Key is the condition name, label key, or probe name. Required for all
	// source types except Prometheus.
	// +optional
	Key string `json:"key,omitempty"`
	// Query is the PromQL expression. Required for Prometheus sources.
	// +optional
	Query string `json:"query,omitempty"`
}

// SignalSpec is one health rule.
type SignalSpec struct {
	// +kubebuilder:validation:MinLength=1
	Name   string       `json:"name"`
	Source SignalSource `json:"source"`

	// +kubebuilder:validation:Enum=GreaterThan;LessThan;Equals;NotEquals
	Operator string `json:"operator"`

	Threshold string `json:"threshold"`

	// Window is how long a breach must persist before it counts. A single ECC
	// error is noise; five in ten minutes is a dying card.
	// +optional
	Window *metav1.Duration `json:"window,omitempty"`

	// RequireConsecutive adds hysteresis against single-scrape spikes.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RequireConsecutive int `json:"requireConsecutive,omitempty"`

	// +kubebuilder:validation:Enum=Info;Warning;Critical
	// +kubebuilder:default=Warning
	Severity string `json:"severity"`

	// +kubebuilder:validation:Enum=Taint;Cordon;Drain;Quarantine
	Action string `json:"action"`
}

// EscalationSpec tunes the lifecycle state machine dwell times.
type EscalationSpec struct {
	// +kubebuilder:default="5m"
	// +optional
	GracePeriod *metav1.Duration `json:"gracePeriod,omitempty"`
	// DrainTimeout bounds how long a drain may run before the node is forced
	// terminal, so a pod with a broken PDB cannot pin a budget slot forever.
	// +kubebuilder:default="15m"
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`
}

// RepairSpec controls quarantine behaviour.
type RepairSpec struct {
	// +kubebuilder:default="fleet.elbadry.dev/repair-required"
	// +optional
	QuarantineLabel string `json:"quarantineLabel,omitempty"`
	// RequireManualClear prevents automatic return to service. Defaults true
	// because auto-clearing masks recurring hardware faults.
	// +kubebuilder:default=true
	// +optional
	RequireManualClear bool `json:"requireManualClear,omitempty"`
}

// NodeHealthPolicySpec defines health rules for one node class.
type NodeHealthPolicySpec struct {
	NodeClassRef corev1LocalRef `json:"nodeClassRef"`

	// +kubebuilder:validation:MinItems=1
	Signals []SignalSpec `json:"signals"`

	// +optional
	Escalation *EscalationSpec `json:"escalation,omitempty"`
	// +optional
	Repair *RepairSpec `json:"repair,omitempty"`
}

// corev1LocalRef is a name-only reference to a cluster-scoped object.
type corev1LocalRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// NodeHealthPolicyStatus reports policy evaluation state.
type NodeHealthPolicyStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	NodesEvaluated int `json:"nodesEvaluated"`
	// +optional
	ActiveBreaches int `json:"activeBreaches"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nhp
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.nodeClassRef.name`
// +kubebuilder:printcolumn:name="Evaluated",type=integer,JSONPath=`.status.nodesEvaluated`
// +kubebuilder:printcolumn:name="Breaching",type=integer,JSONPath=`.status.activeBreaches`

// NodeHealthPolicy declares health signals and escalation for a node class.
type NodeHealthPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   NodeHealthPolicySpec   `json:"spec,omitempty"`
	Status NodeHealthPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// NodeHealthPolicyList is a list of NodeHealthPolicy.
type NodeHealthPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeHealthPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&NodeClass{}, &NodeClassList{},
		&NodeHealthPolicy{}, &NodeHealthPolicyList{},
	)
}
