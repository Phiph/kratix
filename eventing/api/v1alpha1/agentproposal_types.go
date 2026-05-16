package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentProposalSpec is the immutable description of a proposed action.
// It is set once when the gate controller materialises a .proposed event
// and never modified afterwards — the spec is the audit object.
type AgentProposalSpec struct {
	// ProposedEventType is the CloudEvent type that originated this
	// proposal (e.g. agent.redis.failover.proposed).
	// +kubebuilder:validation:Required
	ProposedEventType string `json:"proposedEventType"`

	// Actor identifies the agent that proposed the action.
	// Format is free-form but conventionally
	// "agent/<name>/<version>" (e.g. "agent/redis-flake-detector/v1.2.0").
	// +kubebuilder:validation:Required
	Actor string `json:"actor"`

	// Subject is the CloudEvent subject of the proposal: the object the
	// proposed action would affect (e.g. "default/promise/redis-primary").
	// +kubebuilder:validation:Required
	Subject string `json:"subject"`

	// Action is a short verb describing what is proposed (e.g. "failover",
	// "reset-pause-label"). Human-readable; used by UIs and runbooks.
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// Rationale is the agent's explanation of why this action is being
	// proposed. Approvers read this before annotating.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Rationale string `json:"rationale"`

	// Evidence carries the trail behind the rationale. v0.1 keeps this
	// open-ended (raw JSON bytes); future revisions may tighten it.
	// +optional
	Evidence *AgentProposalEvidence `json:"evidence,omitempty"`

	// Plan is the agent-specific blob describing how the action will be
	// executed. Opaque to Kratix; consumed only by the proposing agent.
	// Carried as raw JSON to keep the API surface narrow.
	// +optional
	Plan *AgentProposalPlan `json:"plan,omitempty"`

	// CorrelationID is the CloudEvent correlation ID of the originating
	// .proposed event. Useful for tracing back to the reconcile loops the
	// agent observed.
	// +optional
	CorrelationID string `json:"correlationId,omitempty"`

	// ExpiresAt is the deadline after which an approval is no longer
	// honoured. The gate controller emits .expired and resolves the
	// proposal automatically when this time passes.
	// +kubebuilder:validation:Required
	ExpiresAt metav1.Time `json:"expiresAt"`
}

// AgentProposalEvidence carries the supporting evidence for a proposal.
// Free-form by design — v0.1 does not constrain producers beyond "include
// enough that an approver can decide".
type AgentProposalEvidence struct {
	// CorrelationIDs links to other CloudEvent correlation IDs the agent
	// observed when forming this proposal.
	// +optional
	CorrelationIDs []string `json:"correlationIds,omitempty"`

	// Since is the timestamp the agent began observing the pattern that
	// led to this proposal.
	// +optional
	Since *metav1.Time `json:"since,omitempty"`

	// Notes is a free-form scratchpad for additional context. JSON-encoded
	// to allow agents to attach structured data without expanding the API.
	// +optional
	Notes string `json:"notes,omitempty"`
}

// AgentProposalPlan is the opaque execution plan supplied by the proposing
// agent. The gate controller never inspects it; only the agent that
// emitted the .proposed event understands its contents.
type AgentProposalPlan struct {
	// Body is a JSON-encoded blob. The shape is agent-specific.
	// +optional
	Body string `json:"body,omitempty"`
}

// AgentProposalResolution is the terminal state of a proposal.
// +kubebuilder:validation:Enum=approved;expired;executed
type AgentProposalResolution string

const (
	AgentProposalResolutionApproved AgentProposalResolution = "approved"
	AgentProposalResolutionExpired  AgentProposalResolution = "expired"
	AgentProposalResolutionExecuted AgentProposalResolution = "executed"
)

// ApprovalAnnotation is the annotation key approvers set to approve a
// proposal. The gate controller watches for this annotation appearing on
// an AgentProposal and emits the matching .approved CloudEvent.
const ApprovalAnnotation = "agents.kratix.io/approved-by"

// AgentProposalStatus reports the observed state of a proposal.
type AgentProposalStatus struct {
	// Conditions reports observed state. Standard:
	//   Ready  — proposal is alive and awaiting approval.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// ApprovedBy is the value of the agents.kratix.io/approved-by
	// annotation at the moment the gate controller observed approval.
	// Immutable once set — the first observation wins.
	// +optional
	ApprovedBy string `json:"approvedBy,omitempty"`

	// ApprovedAt is the timestamp at which approval was observed.
	// +optional
	ApprovedAt *metav1.Time `json:"approvedAt,omitempty"`

	// Resolution is set once the proposal reaches a terminal state.
	// Empty while the proposal is still awaiting approval.
	// +optional
	Resolution AgentProposalResolution `json:"resolution,omitempty"`

	// ResolvedAt is the timestamp at which Resolution was set.
	// +optional
	ResolvedAt *metav1.Time `json:"resolvedAt,omitempty"`
}

// AgentProposal is the audit object for an agent's proposed action.
// It is created by the escalation gate controller when an .proposed
// CloudEvent is received, and resolved when either approved (via the
// agents.kratix.io/approved-by annotation) or expired.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=aprop
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Actor",type=string,JSONPath=`.spec.actor`
// +kubebuilder:printcolumn:name="Subject",type=string,JSONPath=`.spec.subject`
// +kubebuilder:printcolumn:name="ExpiresAt",type=date,JSONPath=`.spec.expiresAt`
// +kubebuilder:printcolumn:name="Resolution",type=string,JSONPath=`.status.resolution`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type AgentProposal struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentProposalSpec   `json:"spec,omitempty"`
	Status AgentProposalStatus `json:"status,omitempty"`
}

// AgentProposalList contains a list of AgentProposal.
//
// +kubebuilder:object:root=true
type AgentProposalList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentProposal `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentProposal{}, &AgentProposalList{})
}
