package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LabelPrefix starts every label the operator sets. A template may not use it (the API server refuses one that does).
const LabelPrefix = "rollouts.vivekdavara.github.io/"

// CanaryRolloutSpec is what the user asks for: a version to run, and how to move to it.
//
// +kubebuilder:validation:XValidation:rule="!has(self.template.metadata) || !has(self.template.metadata.labels) || self.template.metadata.labels.all(k, !k.startsWith('rollouts.vivekdavara.github.io/'))",message="template labels may not start with rollouts.vivekdavara.github.io/ (the operator sets those)"
type CanaryRolloutSpec struct {
	// Replicas is the number of pods in total, stable and canary together (during a step below 100% that leaves no
	// stable pod, one more).
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +required
	Replicas int32 `json:"replicas"`

	// Template is the version to run. A change to it starts a rollout.
	//
	// +required
	Template corev1.PodTemplateSpec `json:"template"`

	// Steps are the canary's shares of the pods, in percent, strictly increasing. After the last one passes, the
	// canary is promoted.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self.map(s, s.weight).isSorted() && self.all(s, self.exists_one(t, t.weight == s.weight))",message="step weights must be strictly increasing"
	// +listType=atomic
	// +required
	Steps []Step `json:"steps"`

	// ProgressDeadlineSeconds is how long a step's canary pods have to become ready before the rollout rolls back.
	//
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	// +optional
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`

	// Analysis is checked during every step's pause. Without it, steps advance on their pauses alone.
	//
	// +optional
	Analysis *Analysis `json:"analysis,omitempty"`
}

// Step is one stage of a rollout.
type Step struct {
	// Weight is the canary's share of the pods, in percent.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +required
	Weight int32 `json:"weight"`

	// Pause is how long the step holds once its canary pods are ready (a Go duration such as 30s or 5m).
	//
	// +kubebuilder:default="0s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('0s') && duration(self) <= duration('24h')",message="pause must be from 0s to 24h"
	// +optional
	Pause *metav1.Duration `json:"pause,omitempty"`
}

// Analysis is the set of metric checks that decide whether a step passes.
type Analysis struct {
	// Interval is the time between checks (a Go duration).
	//
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1s') && duration(self) <= duration('1h')",message="interval must be from 1s to 1h"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// FailureLimit is the number of failed checks a step tolerates; one more rolls the rollout back.
	//
	// +kubebuilder:default=0
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +optional
	FailureLimit *int32 `json:"failureLimit,omitempty"`

	// Metrics are the queries checked at each interval; a check passes when all of them are within their bounds.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +listType=map
	// +listMapKey=name
	// +required
	Metrics []Metric `json:"metrics"`
}

// Metric is one Prometheus query and the range its result must stay in.
//
// +kubebuilder:validation:XValidation:rule="has(self.min) || has(self.max)",message="a metric needs min, max or both"
// +kubebuilder:validation:XValidation:rule="!has(self.min) || !has(self.max) || quantity(string(self.min)).compareTo(quantity(string(self.max))) <= 0",message="min must not be greater than max"
type Metric struct {
	// Name identifies the metric in status and Events (a DNS label).
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +required
	Name string `json:"name"`

	// Query is a PromQL instant query whose result is one number.
	//
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	// +required
	Query string `json:"query"`

	// Min is the lowest passing value (a Kubernetes quantity: "0.5", "500m", 2).
	//
	// +optional
	Min *resource.Quantity `json:"min,omitempty"`

	// Max is the highest passing value (a Kubernetes quantity: "0.01", "10m", 2).
	//
	// +optional
	Max *resource.Quantity `json:"max,omitempty"`
}

// Phase is where a rollout is (DESIGN.md, "The state machine").
//
// +kubebuilder:validation:Enum=Healthy;Progressing;Promoting;RolledBack
type Phase string

const (
	// PhaseHealthy means the stable runs the template's revision and there is no canary.
	PhaseHealthy Phase = "Healthy"
	// PhaseProgressing means a canary is running at one of the steps.
	PhaseProgressing Phase = "Progressing"
	// PhasePromoting means the last step passed and the stable is moving to the canary's revision.
	PhasePromoting Phase = "Promoting"
	// PhaseRolledBack means the last rollout failed; the stable runs the old revision until the template changes.
	PhaseRolledBack Phase = "RolledBack"
)

// CanaryRolloutStatus is what the operator observed and did. DESIGN.md, "Status", says which day fills each field.
type CanaryRolloutStatus struct {
	// ObservedGeneration is the metadata.generation this status describes.
	//
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UpdateRevision is the revision of spec.template.
	//
	// +optional
	UpdateRevision string `json:"updateRevision,omitempty"`

	// StableRevision is the revision the stable Deployment runs.
	//
	// +optional
	StableRevision string `json:"stableRevision,omitempty"`

	// CanaryRevision is the revision under test; empty when there is none.
	//
	// +optional
	CanaryRevision string `json:"canaryRevision,omitempty"`

	// RolledBackRevision is the last revision rolled back; it isn't retried until the template changes.
	//
	// +optional
	RolledBackRevision string `json:"rolledBackRevision,omitempty"`

	// Phase is where the rollout is.
	//
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// CurrentStep is the index of the step in progress.
	//
	// +optional
	CurrentStep *int32 `json:"currentStep,omitempty"`

	// StepStartedAt is when the current step's canary pods became ready and its pause began.
	//
	// +optional
	StepStartedAt *metav1.Time `json:"stepStartedAt,omitempty"`

	// StableReplicas and CanaryReplicas are the ready pods of each Deployment.
	//
	// +optional
	StableReplicas int32 `json:"stableReplicas,omitempty"`
	// +optional
	CanaryReplicas int32 `json:"canaryReplicas,omitempty"`

	// CanaryWeight is the canary's actual share of the pods, in percent, rounded down.
	//
	// +optional
	CanaryWeight int32 `json:"canaryWeight,omitempty"`

	// Conditions are Progressing, Available and AnalysisPassing.
	//
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// CanaryRollout runs a pod template and moves it to new versions in canary steps.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=crl
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Step",type=integer,JSONPath=`.status.currentStep`
// +kubebuilder:printcolumn:name="Canary %",type=integer,JSONPath=`.status.canaryWeight`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.updateRevision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 50",message="the name may be at most 50 characters (it prefixes Deployment and pod names)"
type CanaryRollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +required
	Spec CanaryRolloutSpec `json:"spec"`

	// +optional
	Status CanaryRolloutStatus `json:"status,omitempty"`
}

// CanaryRolloutList is a list of CanaryRollouts.
//
// +kubebuilder:object:root=true
type CanaryRolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CanaryRollout `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CanaryRollout{}, &CanaryRolloutList{})
}
