package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// cloudEvent is the structured-mode CloudEvents v1.0 envelope the forwarder
// emits over HTTP. JSON tags match the CE spec exactly.
type cloudEvent struct {
	SpecVersion     string `json:"specversion"`
	ID              string `json:"id"`
	Source          string `json:"source"`
	Type            string `json:"type"`
	Subject         string `json:"subject"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`

	InstallID     string `json:"kratixinstallid"`
	CorrelationID string `json:"kratixcorrelationid"`
	Generation    int64  `json:"kratixgeneration"`
	Severity      string `json:"kratixseverity"`

	Data eventData `json:"data"`
}

type eventData struct {
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	APIVersion string `json:"apiversion"`
	Reason     string `json:"reason"`
	Message    string `json:"message,omitempty"`
}

// translate converts a Kubernetes Event into a CloudEvent. It returns
// (nil, "", false) when the Event does not match the Kratix wire format
// — non-Kratix Events, missing required annotations, malformed values.
// The caller is responsible for counting drops; translate itself is pure.
//
// The second return value is a one-word reason ("not-kratix", "missing-annotation",
// "bad-generation") suitable for a drop-counter label.
func translate(ev *corev1.Event, installID string) (*cloudEvent, string, bool) {
	// The annotation set is the producer signal (see WIRE-FORMAT.md §3).
	// Resolve the type from the explicit annotation when present (the path
	// user-emitted pipeline events take); otherwise derive it from the
	// kratix-namespaced reason (the path Kratix controllers take).
	ceType, ok := resolveType(ev)
	if !ok {
		return nil, "not-kratix", false
	}

	corrID, ok := ev.Annotations[schema.AnnotationCorrelationID]
	if !ok || corrID == "" {
		return nil, "missing-annotation", false
	}
	genStr, ok := ev.Annotations[schema.AnnotationGeneration]
	if !ok || genStr == "" {
		return nil, "missing-annotation", false
	}
	gen, err := strconv.ParseInt(genStr, 10, 64)
	if err != nil {
		return nil, "bad-generation", false
	}

	ce := &cloudEvent{
		SpecVersion:     schema.SpecVersion,
		ID:              newCEID(),
		Source:          fmt.Sprintf("/kratix/%s/event-forwarder", installID),
		Type:            ceType,
		Subject:         subjectFromEvent(ev),
		Time:            eventTime(ev).UTC().Format(time.RFC3339Nano),
		DataContentType: "application/json",
		InstallID:       installID,
		CorrelationID:   corrID,
		Generation:      gen,
		Severity:        schema.SeverityFromEventType(ev.Type),
		Data: eventData{
			Namespace:  ev.InvolvedObject.Namespace,
			Name:       ev.InvolvedObject.Name,
			Kind:       ev.InvolvedObject.Kind,
			APIVersion: ev.InvolvedObject.APIVersion,
			Reason:     ev.Reason,
			Message:    ev.Message,
		},
	}
	return ce, "", true
}

// resolveType picks the authoritative CE type for an Event. It returns false
// when neither path yields a Kratix-shaped type — that's the forwarder's
// signal to ignore the Event (non-Kratix traffic).
func resolveType(ev *corev1.Event) (string, bool) {
	if t := ev.Annotations[schema.AnnotationType]; t != "" {
		// Producer set the type explicitly. Trust it verbatim — this is how
		// user-emitted pipeline events stay out of the kratix.* namespace.
		return t, true
	}
	return schema.ReasonToType(ev.Reason)
}

func subjectFromEvent(ev *corev1.Event) string {
	kind := strings.ToLower(ev.InvolvedObject.Kind)
	name := ev.InvolvedObject.Name
	ns := ev.InvolvedObject.Namespace
	if ns == "" {
		return fmt.Sprintf("%s/%s", kind, name)
	}
	return fmt.Sprintf("%s/%s/%s", ns, kind, name)
}

func eventTime(ev *corev1.Event) time.Time {
	if !ev.EventTime.IsZero() {
		return ev.EventTime.Time
	}
	if !ev.LastTimestamp.IsZero() {
		return ev.LastTimestamp.Time
	}
	return time.Now()
}

// newCEID generates a fresh CloudEvent id. ULID-ish but we keep it dependency-
// free: 16 random bytes, hex-encoded. Uniqueness is what matters; ordering of
// IDs is not part of the wire contract.
func newCEID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
