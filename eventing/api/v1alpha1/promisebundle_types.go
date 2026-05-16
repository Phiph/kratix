package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PromiseBundleSpec declares the resources that ship alongside a Promise.
//
// Where a Promise's `spec` is the platform contract (the CRD the producer
// promises to fulfil), a PromiseBundle is the producer's manifest of
// *companions*: the agents, configs, and supporting resources the Promise
// brings with it.
//
// The bundle controller applies these resources when the referenced
// Promise becomes Available. The Promise's `dependencies:` field is for
// other Promises; this is for everything else.
type PromiseBundleSpec struct {
	// PromiseRef names the Promise this bundle ships alongside.
	// +kubebuilder:validation:Required
	PromiseRef PromiseBundlePromiseRef `json:"promiseRef"`

	// Companions is the ordered list of resources to apply once the
	// Promise reaches Available. Apply order is preserved so producers
	// can stage dependencies (e.g. ConfigMap before Deployment).
	// +kubebuilder:validation:Required
	// +listType=atomic
	Companions []Companion `json:"companions"`
}

// PromiseBundlePromiseRef is a minimal cross-reference to a Promise.
type PromiseBundlePromiseRef struct {
	// Name of the Promise (cluster-scoped).
	// +kubebuilder:validation:Required
	Name string `json:"name"`
}

// Companion is one resource the bundle ships. Exactly one of `inline` or
// `ref` MUST be set. The bundle controller enforces this at apply time
// (decodeCompanion rejects mismatched companions with a non-Applied
// status). CEL-level enforcement isn't viable here because `inline` is
// schemaless (preserve-unknown-fields), which CEL cannot reason about.
type Companion struct {
	// Name is a stable identifier for this companion within the bundle.
	// Used in status reporting and to provide deterministic ordering.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Inline carries the resource manifest verbatim. The bundle
	// controller decodes it as unstructured.Unstructured at apply time;
	// the producer is responsible for the manifest's correctness.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	Inline *runtime.RawExtension `json:"inline,omitempty"`

	// Ref refers to a resource the controller should fetch and apply.
	// v0.1 supports a single shape: an in-cluster ConfigMap holding the
	// manifest as YAML. Future revisions may add HTTP/git references.
	// +optional
	Ref *CompanionRef `json:"ref,omitempty"`
}

// CompanionRef references a resource the controller resolves at apply
// time. v0.1 supports ConfigMaps only — producers ship the manifest
// content as a ConfigMap key. This keeps the controller's threat model
// inside-the-cluster.
type CompanionRef struct {
	// ConfigMap is the reference shape. The named ConfigMap's `.data[key]`
	// is parsed as a YAML document.
	// +kubebuilder:validation:Required
	ConfigMap CompanionConfigMapRef `json:"configMap"`
}

type CompanionConfigMapRef struct {
	// Name of the ConfigMap.
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// Namespace of the ConfigMap.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`
	// Key within the ConfigMap's `data` map.
	// +kubebuilder:validation:Required
	Key string `json:"key"`
}

// CompanionApplyResult is the outcome of one companion's most recent apply
// attempt.
// +kubebuilder:validation:Enum=applied;failed;pending
type CompanionApplyResult string

const (
	CompanionApplied CompanionApplyResult = "applied"
	CompanionFailed  CompanionApplyResult = "failed"
	CompanionPending CompanionApplyResult = "pending"
)

// CompanionStatus is per-companion observed state.
type CompanionStatus struct {
	// Name mirrors Companion.Name; correlates by ordinal+name.
	Name string `json:"name"`
	// Result is the most recent apply outcome.
	Result CompanionApplyResult `json:"result"`
	// Kind/APIVersion/Namespace/ResourceName describe the applied
	// object. Filled in once we successfully decode the manifest, even
	// if the apply itself failed.
	// +optional
	APIVersion string `json:"apiVersion,omitempty"`
	// +optional
	Kind string `json:"kind,omitempty"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	ResourceName string `json:"resourceName,omitempty"`
	// Message is human-readable detail (failure cause, etc.).
	// +optional
	Message string `json:"message,omitempty"`
	// LastAppliedAt is when the controller last successfully applied
	// this companion.
	// +optional
	LastAppliedAt *metav1.Time `json:"lastAppliedAt,omitempty"`
}

// PromiseBundleStatus reports observed state.
type PromiseBundleStatus struct {
	// Conditions standard set:
	//   Ready  — true when the Promise is Available and every companion
	//            applied successfully on the most recent reconcile.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Companions is one entry per spec.companions entry, in the same
	// order. Each carries the most recent apply outcome.
	// +optional
	// +listType=atomic
	Companions []CompanionStatus `json:"companions,omitempty"`

	// ObservedGeneration is the spec generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// PromiseBundle declares the companion resources that ship alongside a
// Promise — agents, signal taxonomies, configs, supporting Deployments.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pbun
// +kubebuilder:printcolumn:name="Promise",type=string,JSONPath=`.spec.promiseRef.name`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PromiseBundle struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PromiseBundleSpec   `json:"spec,omitempty"`
	Status PromiseBundleStatus `json:"status,omitempty"`
}

// PromiseBundleList contains a list of PromiseBundle.
//
// +kubebuilder:object:root=true
type PromiseBundleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PromiseBundle `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PromiseBundle{}, &PromiseBundleList{})
}
