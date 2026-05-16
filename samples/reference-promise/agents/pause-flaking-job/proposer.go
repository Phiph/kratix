package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// proposer emits agent.scheduledjob.pause.proposed CloudEvents. It does
// not interact with AgentProposal CRs directly — the gate controller
// materialises them. The proposer is a *producer* of .proposed events.
type proposer struct {
	kc            kubernetes.Interface
	emitNamespace string // where the annotated K8s Events are created
	proposalNS    string // namespace where the gate controller materialises AgentProposals
	actor         string
	expiry        time.Duration
	now           func() time.Time
}

func newProposer(kc kubernetes.Interface, emitNamespace, proposalNamespace, actor string, expiry time.Duration) *proposer {
	return &proposer{
		kc:            kc,
		emitNamespace: emitNamespace,
		proposalNS:    proposalNamespace,
		actor:         actor,
		expiry:        expiry,
		now:           time.Now,
	}
}

// Propose emits a .proposed CE for a flaking ScheduledJob. The payload
// carries everything an approver needs to evaluate: the schedule's
// identity, the failure count, the observation window, and a hint at the
// kubectl command that would otherwise do the job.
//
// Returns the proposalId so the caller can correlate the later
// .approved / .expired events.
func (p *proposer) Propose(ctx context.Context, subject, scheduledJobName, scheduledJobNamespace string, failures int, windowMinutes int) (string, error) {
	proposalID := newProposalID()
	now := p.now()
	expiresAt := now.Add(p.expiry)

	payload := map[string]any{
		"action":     "pause-scheduled-job",
		"actor":      p.actor,
		"subject":    subject,
		"rationale":  fmt.Sprintf("%d failures observed in the last %d minutes", failures, windowMinutes),
		"proposalId": proposalID,
		"expiresAt":  expiresAt.UTC().Format(time.RFC3339),
		"evidence": map[string]any{
			"notes": fmt.Sprintf("ScheduledJob %s/%s has failed %d times in the rolling window.", scheduledJobNamespace, scheduledJobName, failures),
		},
		"plan": map[string]any{
			"kubectl":      fmt.Sprintf("kubectl -n %s patch scheduledjob %s --type=merge -p '{\"spec\":{\"suspended\":true}}'", scheduledJobNamespace, scheduledJobName),
			"scheduledJob": map[string]any{
				"name":      scheduledJobName,
				"namespace": scheduledJobNamespace,
			},
		},
	}
	dataJSON, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	t := metav1.NewTime(now)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pause-flaking-job-proposed-",
			Namespace:    p.emitNamespace,
			Annotations: map[string]string{
				"kratix.io/ce-correlation-id": newCorrelationID(),
				"kratix.io/ce-generation":     strconv.FormatInt(0, 10),
				"kratix.io/ce-type":           "agent.scheduledjob.pause.proposed",
				"kratix.io/ce-data":           string(dataJSON),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "ScheduledJob",
			APIVersion: "platform.kratix.io/v1alpha1",
			Name:       scheduledJobName,
			Namespace:  scheduledJobNamespace,
		},
		Reason:              "ScheduledjobPauseProposed",
		Message:             fmt.Sprintf("Proposing to pause ScheduledJob %s (%d failures in window)", scheduledJobName, failures),
		Type:                corev1.EventTypeWarning,
		Source:              corev1.EventSource{Component: "pause-flaking-job-agent"},
		FirstTimestamp:      t,
		LastTimestamp:       t,
		EventTime:           metav1.NewMicroTime(t.Time),
		ReportingController: "pause-flaking-job-agent",
		ReportingInstance:   p.actor,
		Action:              "ScheduledjobPauseProposed",
	}
	if _, err := p.kc.CoreV1().Events(p.emitNamespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("create proposed event: %w", err)
	}
	return proposalID, nil
}

func newProposalID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return "psj-" + hex.EncodeToString(b[:])
}

func newCorrelationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
