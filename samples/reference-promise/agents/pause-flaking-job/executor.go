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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// executor applies the proposed action once approval has landed.
// "Action" for this agent is: set spec.suspended=true on the
// ScheduledJob. We use the dynamic client so we don't need to register
// the ScheduledJob types into a scheme.
type executor struct {
	dc            dynamic.Interface
	kc            kubernetes.Interface
	emitNamespace string
	actor         string
	now           func() time.Time
}

func newExecutor(dc dynamic.Interface, kc kubernetes.Interface, emitNamespace, actor string) *executor {
	return &executor{
		dc:            dc,
		kc:            kc,
		emitNamespace: emitNamespace,
		actor:         actor,
		now:           time.Now,
	}
}

var scheduledJobGVR = schema.GroupVersionResource{
	Group:    "platform.kratix.io",
	Version:  "v1alpha1",
	Resource: "scheduledjobs",
}

// Execute patches the ScheduledJob spec.suspended=true and emits the
// matching .executed CloudEvent. Best-effort idempotent: if the field is
// already true, the patch is a no-op and we still emit (the approval is
// the contract; the agent records that it acted on it).
func (e *executor) Execute(ctx context.Context, proposalID, scheduledJobName, scheduledJobNamespace, correlationID string) error {
	patch := []byte(`{"spec":{"suspended":true}}`)
	_, err := e.dc.Resource(scheduledJobGVR).
		Namespace(scheduledJobNamespace).
		Patch(ctx, scheduledJobName, types.MergePatchType, patch, metav1.PatchOptions{FieldManager: "pause-flaking-job-agent"})
	if err != nil {
		// Emit a follow-up .executed CE with the failure outcome so the
		// audit trail closes either way. The gate contract requires
		// .executed to be emitted regardless of outcome.
		_ = e.emitExecuted(ctx, proposalID, scheduledJobName, scheduledJobNamespace, correlationID, "failed", err.Error())
		return fmt.Errorf("patch ScheduledJob: %w", err)
	}
	return e.emitExecuted(ctx, proposalID, scheduledJobName, scheduledJobNamespace, correlationID, "succeeded", "")
}

func (e *executor) emitExecuted(ctx context.Context, proposalID, scheduledJobName, scheduledJobNamespace, correlationID, outcome, errMsg string) error {
	payload := map[string]any{
		"proposalId":   proposalID,
		"scheduledJob": fmt.Sprintf("%s/%s", scheduledJobNamespace, scheduledJobName),
		"outcome":      outcome,
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	dataJSON, _ := json.Marshal(payload)

	now := metav1.NewTime(e.now())
	corr := correlationID
	if corr == "" {
		corr = newCorrelationIDExec()
	}
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pause-flaking-job-executed-",
			Namespace:    e.emitNamespace,
			Annotations: map[string]string{
				"kratix.io/ce-correlation-id": corr,
				"kratix.io/ce-generation":     strconv.FormatInt(0, 10),
				"kratix.io/ce-type":           "agent.scheduledjob.pause.executed",
				"kratix.io/ce-data":           string(dataJSON),
			},
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "ScheduledJob",
			APIVersion: "platform.kratix.io/v1alpha1",
			Name:       scheduledJobName,
			Namespace:  scheduledJobNamespace,
		},
		Reason:              "ScheduledjobPauseExecuted",
		Message:             fmt.Sprintf("Paused ScheduledJob %s (proposalId=%s, outcome=%s)", scheduledJobName, proposalID, outcome),
		Type:                corev1.EventTypeWarning,
		Source:              corev1.EventSource{Component: "pause-flaking-job-agent"},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: "pause-flaking-job-agent",
		ReportingInstance:   e.actor,
		Action:              "ScheduledjobPauseExecuted",
	}
	_, err := e.kc.CoreV1().Events(e.emitNamespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

func newCorrelationIDExec() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
