package schema

import (
	"strings"
	"unicode"
)

// ReasonToType converts a PascalCase Event.reason into a CloudEvent type.
// It splits on capital letters, lower-cases each segment, and joins with
// dots under the kratix.* namespace.
//
// Examples:
//
//	"PromiseUnavailable"      -> "kratix.promise.unavailable"
//	"WorkPlacementWriteFailed" -> "kratix.work.placement.write.failed"
//
// Reasons that do not begin with an upper-case letter are rejected (ok=false)
// — they are not Kratix-namespaced and must be ignored by the forwarder.
func ReasonToType(reason string) (string, bool) {
	if reason == "" {
		return "", false
	}
	if !unicode.IsUpper(rune(reason[0])) {
		return "", false
	}

	var segments []string
	var current strings.Builder
	for i, r := range reason {
		if unicode.IsUpper(r) && i > 0 {
			segments = append(segments, strings.ToLower(current.String()))
			current.Reset()
		}
		current.WriteRune(r)
	}
	if current.Len() > 0 {
		segments = append(segments, strings.ToLower(current.String()))
	}

	return "kratix." + strings.Join(segments, "."), true
}

// SeverityFromEventType maps a Kubernetes Event.type ("Normal" / "Warning")
// to the kratixseverity extension value. Unknown values default to info.
func SeverityFromEventType(eventType string) string {
	switch eventType {
	case "Warning":
		return SeverityWarning
	default:
		return SeverityInfo
	}
}
