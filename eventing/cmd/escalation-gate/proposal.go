package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

// proposedEvent is the minimal CloudEvent shape the gate accepts on
// /proposals. We unmarshal only what we need; unknown fields pass through.
type proposedEvent struct {
	Type          string          `json:"type"`
	Subject       string          `json:"subject"`
	Time          string          `json:"time"`
	CorrelationID string          `json:"kratixcorrelationid"`
	Data          json.RawMessage `json:"data"`
}

// proposalPayload is what we expect to find inside the CE `data` block
// (per docs/escalation-contract.md §3). Required fields are enforced.
type proposalPayload struct {
	Action     string                 `json:"action"`
	Actor      string                 `json:"actor"`
	Subject    string                 `json:"subject"`
	Rationale  string                 `json:"rationale"`
	ProposalID string                 `json:"proposalId"`
	ExpiresAt  string                 `json:"expiresAt"`
	Evidence   *proposalEvidenceInput `json:"evidence,omitempty"`
	Plan       json.RawMessage        `json:"plan,omitempty"`
}

type proposalEvidenceInput struct {
	CorrelationIDs []string `json:"correlationIds,omitempty"`
	Since          string   `json:"since,omitempty"`
	Notes          string   `json:"notes,omitempty"`
}

// errReject indicates the request is structurally invalid — the gate
// returns 400. Any other error is treated as transient (500) by the caller.
type errReject struct{ msg string }

func (e errReject) Error() string { return e.msg }

func reject(format string, args ...any) error {
	return errReject{msg: fmt.Sprintf(format, args...)}
}

// proposalFromCE parses a CE envelope and its embedded payload into the
// fields an AgentProposal needs. Returns errReject for invalid input —
// the caller maps that to a 400.
func proposalFromCE(body []byte, namespace string) (eventingv1alpha1.AgentProposal, error) {
	var ce proposedEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		return eventingv1alpha1.AgentProposal{}, reject("malformed cloudevent: %v", err)
	}
	if !strings.HasSuffix(ce.Type, ".proposed") {
		return eventingv1alpha1.AgentProposal{}, reject("type %q does not end in .proposed", ce.Type)
	}
	if !strings.HasPrefix(ce.Type, "agent.") {
		return eventingv1alpha1.AgentProposal{}, reject("type %q must start with agent.* (reserved namespace)", ce.Type)
	}
	if len(ce.Data) == 0 {
		return eventingv1alpha1.AgentProposal{}, reject("cloudevent data is required for proposals")
	}

	var p proposalPayload
	if err := json.Unmarshal(ce.Data, &p); err != nil {
		return eventingv1alpha1.AgentProposal{}, reject("malformed proposal payload: %v", err)
	}
	if p.ProposalID == "" {
		return eventingv1alpha1.AgentProposal{}, reject("proposalId is required")
	}
	if p.Action == "" || p.Actor == "" || p.Subject == "" || p.Rationale == "" {
		return eventingv1alpha1.AgentProposal{}, reject("action, actor, subject and rationale are required")
	}
	if p.ExpiresAt == "" {
		return eventingv1alpha1.AgentProposal{}, reject("expiresAt is required")
	}
	expiresAt, err := time.Parse(time.RFC3339, p.ExpiresAt)
	if err != nil {
		return eventingv1alpha1.AgentProposal{}, reject("expiresAt %q: %v", p.ExpiresAt, err)
	}

	prop := eventingv1alpha1.AgentProposal{}
	prop.SetName(p.ProposalID)
	prop.SetNamespace(namespace)
	prop.Spec = eventingv1alpha1.AgentProposalSpec{
		ProposedEventType: ce.Type,
		Actor:             p.Actor,
		Subject:           p.Subject,
		Action:            p.Action,
		Rationale:         p.Rationale,
		CorrelationID:     ce.CorrelationID,
		ExpiresAt:         metav1.NewTime(expiresAt),
	}

	if p.Evidence != nil {
		ev := &eventingv1alpha1.AgentProposalEvidence{
			CorrelationIDs: p.Evidence.CorrelationIDs,
			Notes:          p.Evidence.Notes,
		}
		if p.Evidence.Since != "" {
			t, err := time.Parse(time.RFC3339, p.Evidence.Since)
			if err != nil {
				return eventingv1alpha1.AgentProposal{}, reject("evidence.since %q: %v", p.Evidence.Since, err)
			}
			mt := metav1.NewTime(t)
			ev.Since = &mt
		}
		prop.Spec.Evidence = ev
	}
	if len(p.Plan) > 0 {
		prop.Spec.Plan = &eventingv1alpha1.AgentProposalPlan{Body: string(p.Plan)}
	}
	return prop, nil
}

// approvedTypeForProposed converts agent.<domain>.<action>.proposed →
// agent.<domain>.<action>.approved. Pure, used by the controller when
// emitting the .approved CE.
func approvedTypeForProposed(proposed string) string {
	return swapSuffix(proposed, ".proposed", ".approved")
}

// expiredTypeForProposed is the analogous helper for .expired.
func expiredTypeForProposed(proposed string) string {
	return swapSuffix(proposed, ".proposed", ".expired")
}

func swapSuffix(s, oldSuf, newSuf string) string {
	if !strings.HasSuffix(s, oldSuf) {
		return s
	}
	return strings.TrimSuffix(s, oldSuf) + newSuf
}

// isReject reports whether err originated from input validation.
func isReject(err error) bool {
	var r errReject
	return errors.As(err, &r)
}
