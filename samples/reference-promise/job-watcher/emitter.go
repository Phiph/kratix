package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// emitter writes a transition to the apiserver as an annotated K8s Event.
// The forwarder picks it up and fans out to CloudEventSinks. No HTTP
// surface here; the forwarder is the bridge.
type emitter struct {
	kc        kubernetes.Interface
	namespace string
	component string
	instance  string
	now       func() time.Time
}

func newEmitter(kc kubernetes.Interface, namespace string) *emitter {
	host, _ := osHostname()
	return &emitter{
		kc:        kc,
		namespace: namespace,
		component: "scheduledjob-job-watcher",
		instance:  host,
		now:       time.Now,
	}
}

func (e *emitter) Emit(ctx context.Context, t *transition, involved corev1.ObjectReference) error {
	if t == nil {
		return nil
	}
	now := metav1.NewTime(e.now())
	dataJSON, _ := json.Marshal(t.Data)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "scheduledjob-",
			Namespace:    e.namespace,
			Annotations: map[string]string{
				"kratix.io/ce-correlation-id": newCorrelationID(),
				"kratix.io/ce-generation":     strconv.FormatInt(0, 10),
				"kratix.io/ce-type":           t.Type,
				"kratix.io/ce-data":           string(dataJSON),
			},
		},
		InvolvedObject:      involved,
		Reason:              t.Reason,
		Message:             t.Message,
		Type:                eventType(t.Severity),
		Source:              corev1.EventSource{Component: e.component},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: e.component,
		ReportingInstance:   e.instance,
		Action:              t.Reason,
	}
	_, err := e.kc.CoreV1().Events(e.namespace).Create(ctx, ev, metav1.CreateOptions{})
	return err
}

func eventType(severity string) string {
	if severity == "warning" {
		return corev1.EventTypeWarning
	}
	return corev1.EventTypeNormal
}

func newCorrelationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
