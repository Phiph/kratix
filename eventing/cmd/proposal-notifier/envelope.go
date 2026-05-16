package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// proposedEvent is the slice of the CloudEvent the notifier reads. We
// unmarshal only what's needed for the outgoing envelope; unknown fields
// pass through harmlessly.
type proposedEvent struct {
	Type          string          `json:"type"`
	Subject       string          `json:"subject"`
	Time          string          `json:"time"`
	CorrelationID string          `json:"kratixcorrelationid"`
	Data          json.RawMessage `json:"data"`
}

type proposalPayload struct {
	Action     string `json:"action"`
	Actor      string `json:"actor"`
	Subject    string `json:"subject"`
	Rationale  string `json:"rationale"`
	ProposalID string `json:"proposalId"`
	ExpiresAt  string `json:"expiresAt"`
}

// genericEnvelope is the format the relay POSTs to targetURL. Deliberately
// generic: any tool that accepts JSON HTTP posts (Slack incoming webhook,
// Teams workflow, n8n, custom UIs) can read it without per-tool code.
type genericEnvelope struct {
	Event         string `json:"event"`
	Action        string `json:"action"`
	Actor         string `json:"actor"`
	Subject       string `json:"subject"`
	Rationale     string `json:"rationale"`
	ProposalID    string `json:"proposalId"`
	Namespace     string `json:"namespace"`
	ExpiresAt     string `json:"expiresAt"`
	ExpiresIn     string `json:"expiresIn"` // human-friendly "12m30s"
	CorrelationID string `json:"correlationId"`
	ApproveCmd    string `json:"approveCmd"`
	KubectlCmd    string `json:"kubectlCmd"`
}

// errReject signals input the relay deliberately drops (with a log).
// Anything else is treated as transient.
type errReject struct{ msg string }

func (e errReject) Error() string { return e.msg }

func reject(format string, args ...any) error {
	return errReject{msg: fmt.Sprintf(format, args...)}
}

func isReject(err error) bool {
	var r errReject
	return errors.As(err, &r)
}

// transform parses a CloudEvent body and produces the generic envelope.
// Returns errReject for inputs that should be silently dropped (non-
// proposed types, malformed bodies).
//
// `namespace` is the namespace AgentProposals are created in — used to
// build the kubectl helper hint.
//
// `now` makes "expiresIn" deterministic for tests.
func transform(body []byte, namespace string, now time.Time) (genericEnvelope, error) {
	var ce proposedEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		return genericEnvelope{}, reject("malformed cloudevent: %v", err)
	}
	if !strings.HasPrefix(ce.Type, "agent.") || !strings.HasSuffix(ce.Type, ".proposed") {
		return genericEnvelope{}, reject("type %q is not agent.*.proposed", ce.Type)
	}
	if len(ce.Data) == 0 {
		return genericEnvelope{}, reject("cloudevent data is required")
	}

	var p proposalPayload
	if err := json.Unmarshal(ce.Data, &p); err != nil {
		return genericEnvelope{}, reject("malformed proposal payload: %v", err)
	}
	if p.ProposalID == "" {
		return genericEnvelope{}, reject("proposalId is required")
	}

	expiresIn := ""
	if p.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, p.ExpiresAt); err == nil {
			expiresIn = humaniseDuration(t.Sub(now))
		}
	}

	return genericEnvelope{
		Event:         ce.Type,
		Action:        p.Action,
		Actor:         p.Actor,
		Subject:       p.Subject,
		Rationale:     p.Rationale,
		ProposalID:    p.ProposalID,
		Namespace:     namespace,
		ExpiresAt:     p.ExpiresAt,
		ExpiresIn:     expiresIn,
		CorrelationID: ce.CorrelationID,
		ApproveCmd:    fmt.Sprintf("kratix-approve %s --namespace=%s --approver=<you>", p.ProposalID, namespace),
		KubectlCmd:    fmt.Sprintf("kubectl -n %s get agentproposal %s -o yaml", namespace, p.ProposalID),
	}, nil
}

// humaniseDuration produces compact relative-time strings like "12m30s"
// or "expired 3m ago". Used for at-a-glance signalling in notifications.
func humaniseDuration(d time.Duration) string {
	if d < 0 {
		return "expired " + d.Truncate(time.Second).Abs().String() + " ago"
	}
	if d < time.Minute {
		return d.Truncate(time.Second).String()
	}
	return d.Truncate(time.Second).String()
}
