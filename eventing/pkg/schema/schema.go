// Package schema defines the wire-format constants for Kratix eventing.
//
// This package is the authoritative source for annotation keys, CloudEvent
// type names, and severity values. The forwarder imports it. Agents may copy
// these constants verbatim — duplicating two-line string declarations is
// preferred over a cross-repo import dependency, per WIRE-FORMAT.md.
//
// See ../../WIRE-FORMAT.md for the prose contract this codifies.
package schema

// Annotation keys placed on Kubernetes Event objects by Kratix producers.
// All keys live under the kratix.io/ce-* namespace.
const (
	AnnotationCorrelationID = "kratix.io/ce-correlation-id"
	AnnotationGeneration    = "kratix.io/ce-generation"

	// AnnotationType, when present, is the authoritative CloudEvent type.
	// User-emitted pipeline events MUST set it (they emit under the
	// pipeline.* namespace, which cannot be derived from reason naming).
	// Kratix controllers MAY omit it and let ReasonToType derive the type
	// from the kratix-namespaced reason.
	AnnotationType = "kratix.io/ce-type"
)

// RequiredAnnotations is the set of annotation keys the forwarder treats as
// mandatory in v0.1. Events missing any of these are dropped with a counter
// increment.
var RequiredAnnotations = []string{
	AnnotationCorrelationID,
	AnnotationGeneration,
}

// CloudEvent type constants for the transitions wired in v0.1.
// The full transition table lives in the vision doc; this list grows as
// producers add emission.
const (
	TypePromiseUnavailable = "kratix.promise.unavailable"
	TypePromiseAvailable   = "kratix.promise.available"
	TypePromiseReady       = "kratix.promise.ready"
)

// Severity values carried on the kratixseverity CloudEvent extension.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
)

// CloudEvent extension attribute names. Per the CE spec, extensions are
// lower-cased with no separators.
const (
	ExtensionInstallID     = "kratixinstallid"
	ExtensionCorrelationID = "kratixcorrelationid"
	ExtensionGeneration    = "kratixgeneration"
	ExtensionSeverity      = "kratixseverity"
)

// ContentType is the CloudEvents structured-mode content type the forwarder
// emits over HTTP.
const ContentType = "application/cloudevents+json"

// SpecVersion is the CloudEvents spec version the forwarder claims.
const SpecVersion = "1.0"
