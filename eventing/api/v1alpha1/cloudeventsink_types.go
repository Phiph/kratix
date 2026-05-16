package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CloudEventSinkSpec defines an HTTP endpoint that receives Kratix CloudEvents.
type CloudEventSinkSpec struct {
	// URL is the HTTP(S) endpoint the forwarder POSTs CloudEvents to.
	// Must be absolute. The forwarder uses CloudEvents structured-mode
	// (application/cloudevents+json) over HTTP.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// TypeFilter restricts which CloudEvent types this sink receives.
	// Each entry is a glob: an exact match, or a trailing "*" wildcard
	// (e.g. "kratix.promise.*"). If empty, the sink receives all
	// kratix.* events.
	// +optional
	TypeFilter []string `json:"typeFilter,omitempty"`

	// AuthSecretRef optionally references a Secret containing credentials
	// for the sink. The forwarder reads the "Authorization" key from the
	// Secret and sets it verbatim as the HTTP Authorization header.
	// +optional
	AuthSecretRef *corev1.LocalObjectReference `json:"authSecretRef,omitempty"`
}

// CloudEventSinkStatus reports the observed state of a sink.
type CloudEventSinkStatus struct {
	// Conditions represents the latest observations of the sink's state.
	// Standard conditions: Ready (sink reachable and accepting events).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastDeliveryTime is the timestamp of the most recent successful POST.
	// +optional
	LastDeliveryTime *metav1.Time `json:"lastDeliveryTime,omitempty"`

	// ConsecutiveFailures is the number of consecutive failed POSTs since
	// the last success. Used by the forwarder to back off.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
}

// CloudEventSink is the Schema for the cloudeventsinks API.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=ces
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.spec.url`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CloudEventSink struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CloudEventSinkSpec   `json:"spec,omitempty"`
	Status CloudEventSinkStatus `json:"status,omitempty"`
}

// CloudEventSinkList contains a list of CloudEventSink.
//
// +kubebuilder:object:root=true
type CloudEventSinkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CloudEventSink `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CloudEventSink{}, &CloudEventSinkList{})
}
