package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PromiseSignalsSpec declares the CloudEvents a Promise commits to emitting.
// It is producer documentation, expressed as a Kubernetes resource so it
// can be versioned, validated, and queried alongside the Promise itself.
//
// v0.1: documentation-only. No controller reads PromiseSignals yet — the
// resource exists to give producers a contract surface and to enable a
// future event-registry controller. Treat it like an API description, not
// like an active configuration.
type PromiseSignalsSpec struct {
	// PromiseRef names the Promise these signals belong to. Required —
	// signals always attach to a specific Promise.
	// +kubebuilder:validation:Required
	PromiseRef PromiseSignalsPromiseRef `json:"promiseRef"`

	// Owner is a free-form identifier for the team or person responsible
	// for maintaining this taxonomy. Used by registries and runbooks.
	// +optional
	Owner string `json:"owner,omitempty"`

	// Events declares the CloudEvent types this Promise emits. Order is
	// not significant; tools may sort for display.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:Required
	Events []EventDeclaration `json:"events"`
}

// PromiseSignalsPromiseRef is a minimal reference to a Promise. Kept
// separate from corev1.ObjectReference because Promises are cluster-scoped
// and we want a deliberately narrow contract here.
type PromiseSignalsPromiseRef struct {
	// Name is the Promise's metadata.name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// EventDeclaration describes one CloudEvent type the Promise emits.
type EventDeclaration struct {
	// Type is the CloudEvent type, dot-segmented. MUST begin with "kratix."
	// for Promise-emitted events, or with "pipeline." for user-pipeline
	// events. (See eventing/WIRE-FORMAT.md §11.)
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^(kratix|pipeline)\..+`
	Type string `json:"type"`

	// Severity is the kratixseverity extension this type carries when
	// emitted. Determines which agents wake up on it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=info;warning
	Severity string `json:"severity"`

	// Stability indicates the compatibility guarantee for this event type.
	// Producers SHOULD NOT remove or rename stable events without a
	// deprecation cycle.
	//
	//   alpha  — may change or disappear in any release; no guarantees
	//   beta   — will not be renamed within a minor version; payload may change
	//   stable — will not be renamed or removed within a major version
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=alpha;beta;stable
	Stability string `json:"stability"`

	// Description is a short human-readable explanation of when this event
	// fires and what it means. Used by registries and generated docs.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Description string `json:"description"`

	// SinceVersion is the Promise revision in which this type first
	// appeared. Tools use it to render "new in X" markers and to refuse
	// to consume events older than declared.
	// +optional
	SinceVersion string `json:"sinceVersion,omitempty"`

	// DeprecatedSince, if set, marks this type as scheduled for removal.
	// Tools SHOULD warn consumers; producers MUST keep emitting until the
	// removal version is reached.
	// +optional
	DeprecatedSince string `json:"deprecatedSince,omitempty"`

	// Payload describes the shape of the kratix.io/ce-data annotation for
	// this event, if any. v0.1 keeps this open-ended: a map of field-name
	// to type-hint string ("string", "number", "boolean", "object",
	// "array"). Future versions may switch to a JSON Schema subset; the
	// current form is the smallest thing that documents intent.
	// +optional
	Payload map[string]string `json:"payload,omitempty"`
}

// PromiseSignalsStatus is reserved for a future event-registry controller.
// v0.1 leaves it empty; producers should not populate it.
type PromiseSignalsStatus struct {
	// Conditions reports observed state about this declaration. Populated
	// by an event-registry controller, not by producers.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// PromiseSignals declares the CloudEvent taxonomy for a Promise.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=psig
// +kubebuilder:printcolumn:name="Promise",type=string,JSONPath=`.spec.promiseRef.name`
// +kubebuilder:printcolumn:name="Events",type=integer,JSONPath=`.spec.events[*]`,priority=1
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.spec.owner`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PromiseSignals struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromiseSignalsSpec   `json:"spec,omitempty"`
	Status PromiseSignalsStatus `json:"status,omitempty"`
}

// PromiseSignalsList contains a list of PromiseSignals.
//
// +kubebuilder:object:root=true
type PromiseSignalsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromiseSignals `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromiseSignals{}, &PromiseSignalsList{})
}
