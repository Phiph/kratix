package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

// parentRef is everything the CLI needs to attribute an Event to the Kratix
// parent object. Populated from KRATIX_OBJECT_* env vars set by the
// PipelineFactory (api/v1alpha1/pipeline_factory.go:defaultEnvVars).
type parentRef struct {
	Kind       string
	Group      string
	Version    string
	Name       string
	Namespace  string // may be empty for cluster-scoped objects (promises)
	APIVersion string // derived: Group/Version, or just Version for core
}

// readParentFromEnv constructs a parentRef from the KRATIX_OBJECT_* env vars.
// Returns an error when required fields are missing so the caller can decide
// whether to fail hard or fall back silently.
func readParentFromEnv(get func(string) string) (parentRef, error) {
	pr := parentRef{
		Kind:      get("KRATIX_OBJECT_KIND"),
		Group:     get("KRATIX_OBJECT_GROUP"),
		Version:   get("KRATIX_OBJECT_VERSION"),
		Name:      get("KRATIX_OBJECT_NAME"),
		Namespace: get("KRATIX_OBJECT_NAMESPACE"),
	}
	var missing []string
	if pr.Kind == "" {
		missing = append(missing, "KRATIX_OBJECT_KIND")
	}
	if pr.Version == "" {
		missing = append(missing, "KRATIX_OBJECT_VERSION")
	}
	if pr.Name == "" {
		missing = append(missing, "KRATIX_OBJECT_NAME")
	}
	if len(missing) > 0 {
		return parentRef{}, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	if pr.Group == "" {
		pr.APIVersion = pr.Version
	} else {
		pr.APIVersion = pr.Group + "/" + pr.Version
	}
	return pr, nil
}

// emissionArgs is the parsed user input.
type emissionArgs struct {
	Type     string // e.g. "upstream.fetch.failed"
	Reason   string // PascalCase, derived from Type if not provided
	Severity string // "info" or "warning"
	Message  string // free-form, optional
}

// validate normalises and checks the user input. Centralised so both the CLI
// flag layer and any future API can reuse it.
func (a *emissionArgs) validate() error {
	if a.Type == "" {
		return errors.New("--type is required")
	}
	if strings.HasPrefix(a.Type, "kratix.") {
		return errors.New("--type must not use the reserved kratix.* namespace; user pipelines emit under their own namespace")
	}
	switch a.Severity {
	case "", schema.SeverityInfo:
		a.Severity = schema.SeverityInfo
	case schema.SeverityWarning:
		// ok
	default:
		return fmt.Errorf("--severity must be %q or %q", schema.SeverityInfo, schema.SeverityWarning)
	}
	if a.Reason == "" {
		a.Reason = reasonFromType(a.Type)
	}
	return nil
}

// reasonFromType derives a PascalCase Event.reason from a dot-segmented CE type.
// "upstream.fetch.failed" -> "UpstreamFetchFailed". Used as a fallback when the
// caller doesn't supply --reason; the forwarder's translation step round-trips
// this back into the same CE type via schema.ReasonToType.
func reasonFromType(t string) string {
	var b strings.Builder
	for _, segment := range strings.Split(t, ".") {
		if segment == "" {
			continue
		}
		b.WriteString(strings.ToUpper(segment[:1]))
		if len(segment) > 1 {
			b.WriteString(segment[1:])
		}
	}
	return b.String()
}

// eventTypeForSeverity maps the user-facing severity onto the K8s Event.type.
func eventTypeForSeverity(sev string) string {
	if sev == schema.SeverityWarning {
		return corev1.EventTypeWarning
	}
	return corev1.EventTypeNormal
}

// newCorrelationID returns a fresh correlation ID. Each kratix-emit invocation
// is its own correlation event — pipelines that want to group multiple emits
// under one correlation ID should pass --correlation-id explicitly.
func newCorrelationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
