// Package v1alpha1 contains API types for the Kratix eventing subsystem.
//
// +kubebuilder:object:generate=true
// +groupName=eventing.kratix.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "eventing.kratix.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)
