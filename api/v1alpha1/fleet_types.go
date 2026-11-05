package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FleetSpec describes a fleet. The resource's name is the fleet's name in the control plane.
// Devices are still registered with rolloutctl, because their tokens are secrets shown once.
//
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="a fleet's spec can't change once it exists"
type FleetSpec struct {
	// Labels are set on the fleet when it's created.
	// +optional
	// +kubebuilder:validation:MaxProperties=32
	Labels map[string]string `json:"labels,omitempty"`
}

// FleetStatus is what the control plane reports about the fleet.
type FleetStatus struct {
	// ObservedGeneration is the generation this status describes.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ID is the fleet's ID in the control plane.
	// +optional
	ID string `json:"id,omitempty"`
	// Conditions report whether the fleet exists in the control plane.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Fleet is a named group of devices in the control plane. Fleet names are global there, so the
// resource is cluster-scoped. Deleting it stops managing the fleet, and leaves it in place.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="ID",type=string,JSONPath=`.status.id`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$')",message="the name must be a fleet name: lowercase letters, digits, and hyphens, at most 63 characters"
type Fleet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FleetSpec   `json:"spec,omitempty"`
	Status FleetStatus `json:"status,omitempty"`
}

// FleetList is a list of fleets.
//
// +kubebuilder:object:root=true
type FleetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Fleet `json:"items"`
}
