package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ArtifactRef names a signed artifact already pushed to the control plane.
type ArtifactRef struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`
	Version string `json:"version"`
	// Platform is the devices' platform, as os/arch.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+/[a-z0-9]+$`
	Platform string `json:"platform"`
}

// Stage is one step of a rollout.
type Stage struct {
	// Percent of the fleet exposed once this stage opens, counting earlier stages.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percent int `json:"percent"`
	// Soak is how long the stage must stay healthy before the next one opens.
	// +optional
	Soak *metav1.Duration `json:"soak,omitempty"`
	// Deadline is how long the stage may take to reach its coverage and soak.
	// +optional
	Deadline *metav1.Duration `json:"deadline,omitempty"`
	// MinObservations is the fewest outcomes the stage needs before it can pass.
	// +optional
	// +kubebuilder:validation:Minimum=1
	MinObservations *int `json:"minObservations,omitempty"`
}

// Safety holds the limits that halt a rollout. Rates are percentages such as "2%" or
// "0.25%".
type Safety struct {
	// MaxFailureRate is the highest failure rate allowed within a stage's own cohort.
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?%$`
	MaxFailureRate string `json:"maxFailureRate"`
	// RolloutMaxFailureRate is the highest failure rate allowed across every exposed device.
	// It defaults to MaxFailureRate.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?%$`
	RolloutMaxFailureRate string `json:"rolloutMaxFailureRate,omitempty"`
	// RequiredCoverage is the share of a stage that must have an outcome before it can pass.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?%$`
	RequiredCoverage string `json:"requiredCoverage,omitempty"`
	// MaxUnknown is the largest share of devices that may go unheard from.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]{1,2})?%$`
	MaxUnknown string `json:"maxUnknown,omitempty"`
	// StaleAfter is how long without hearing from a device before it counts as unknown.
	// +optional
	StaleAfter *metav1.Duration `json:"staleAfter,omitempty"`
	// OnBreach is what happens when a limit is crossed.
	// +optional
	// +kubebuilder:validation:Enum=Halt;Rollback
	OnBreach string `json:"onBreach,omitempty"`
}

// RolloutSpec describes a staged rollout of one artifact to one fleet. Everything but Paused is
// fixed once the rollout exists, because the control plane freezes a rollout's policy when it
// starts.
//
// +kubebuilder:validation:XValidation:rule="has(self.selector) == has(oldSelf.selector) && has(self.excludeDevices) == has(oldSelf.excludeDevices)",message="only paused can change once a rollout exists"
type RolloutSpec struct {
	// FleetRef is the name of the fleet to roll out to.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	FleetRef string `json:"fleetRef"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	Artifact ArtifactRef `json:"artifact"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	Stages []Stage `json:"stages"`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	Safety Safety `json:"safety"`
	// Selector limits the rollout to devices with all of these labels.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	Selector map[string]string `json:"selector,omitempty"`
	// ExcludeDevices lists devices to leave out. Longer lists can be given through rolloutctl.
	// +optional
	// +kubebuilder:validation:MaxItems=1000
	// +kubebuilder:validation:items:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="only paused can change once a rollout exists"
	ExcludeDevices []string `json:"excludeDevices,omitempty"`
	// Paused pauses a rollout that's progressing, and resumes it when cleared.
	// +optional
	Paused bool `json:"paused,omitempty"`
}

// StageStatus is a stage's progress, as the control plane counts it.
type StageStatus struct {
	Ordinal int    `json:"ordinal"`
	Percent int    `json:"percent"`
	State   string `json:"state"`
	// Cohort is how many devices the stage newly exposes, once it's known.
	// +optional
	Cohort    *int `json:"cohort,omitempty"`
	Succeeded int  `json:"succeeded"`
	Failed    int  `json:"failed"`
	InFlight  int  `json:"inFlight"`
	Pending   int  `json:"pending"`
	Unknown   int  `json:"unknown"`
}

// RollbackStatus is how far a rollback has got. Needed counts devices whose install succeeded,
// Restored those that have confirmed a rollback, and Unresolved the rest.
type RollbackStatus struct {
	Attempt int `json:"attempt"`
	// +optional
	Deadline   *metav1.Time `json:"deadline,omitempty"`
	Needed     int          `json:"needed"`
	Restored   int          `json:"restored"`
	Failed     int          `json:"failed"`
	Unresolved int          `json:"unresolved"`
}

// RolloutStatus is what the control plane reports about the rollout.
type RolloutStatus struct {
	// ObservedGeneration is the generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// RolloutID is the rollout's ID in the control plane, which is this resource's UID.
	// +optional
	RolloutID string `json:"rolloutID,omitempty"`
	// ArtifactID is the ID of the artifact the rollout delivers.
	// +optional
	ArtifactID string `json:"artifactID,omitempty"`
	// Phase is the rollout's state in the control plane, such as running, halted, or completed.
	// +optional
	Phase string `json:"phase,omitempty"`
	// CurrentStage is the ordinal of the stage in progress.
	// +optional
	CurrentStage *int `json:"currentStage,omitempty"`
	// Waiting says what the rollout is waiting for.
	// +optional
	Waiting string `json:"waiting,omitempty"`
	// HaltReason and HaltDetail say why a halted rollout stopped.
	// +optional
	HaltReason string `json:"haltReason,omitempty"`
	// +optional
	HaltDetail string `json:"haltDetail,omitempty"`
	// +optional
	Stages []StageStatus `json:"stages,omitempty"`
	// Rollback is how far a rollback has got, once one has started.
	// +optional
	Rollback *RollbackStatus `json:"rollback,omitempty"`
	// Conditions report whether the rollout exists in the control plane, and anything stopping it.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Rollout is a staged rollout. Applying one creates and starts the rollout in the control
// plane. Deleting one aborts a rollout that hasn't finished. Rollout names are unique per fleet
// in the control plane, and fleets are global, so the resource is cluster-scoped.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Fleet",type=string,JSONPath=`.spec.fleetRef`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Stage",type=integer,JSONPath=`.status.currentStage`
// +kubebuilder:printcolumn:name="Waiting",type=string,JSONPath=`.status.waiting`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Rollout struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RolloutSpec   `json:"spec,omitempty"`
	Status RolloutStatus `json:"status,omitempty"`
}

// RolloutList is a list of rollouts.
//
// +kubebuilder:object:root=true
type RolloutList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Rollout `json:"items"`
}
