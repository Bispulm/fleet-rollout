// Package v1alpha1 holds the Fleet and Rollout custom resources. They're a front end to the
// control plane's API, which stays the source of truth: the operator turns them into API
// calls, and copies what the control plane reports back into their status.
//
// +kubebuilder:object:generate=true
// +groupName=rollouts.bispulm.github.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion identifies these resources.
	GroupVersion = schema.GroupVersion{Group: "rollouts.bispulm.github.io", Version: "v1alpha1"}

	// SchemeBuilder registers these resources with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds these resources to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Fleet{}, &FleetList{}, &Rollout{}, &RolloutList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
