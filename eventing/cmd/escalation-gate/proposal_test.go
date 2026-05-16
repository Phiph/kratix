package main

import (
	"testing"
	"time"
)

const validCE = `{
  "specversion": "1.0",
  "type": "agent.redis.failover.proposed",
  "subject": "default/promise/redis",
  "time": "2026-05-15T19:00:00Z",
  "kratixcorrelationid": "01HZ8W000000000000000000",
  "data": {
    "action": "failover",
    "actor": "agent/redis-flake-detector/v1.2.0",
    "subject": "default/promise/redis",
    "rationale": "3 lag spikes > 30s in last 10m",
    "proposalId": "01HZ9A0000000000000000000",
    "expiresAt": "2026-05-15T19:30:00Z",
    "evidence": {
      "correlationIds": ["01HZ8W..."],
      "since": "2026-05-15T18:50:00Z"
    },
    "plan": {"newPrimary": "replica-2"}
  }
}`

func TestProposalFromCE_Happy(t *testing.T) {
	prop, err := proposalFromCE([]byte(validCE), "observability")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prop.Name != "01HZ9A0000000000000000000" {
		t.Errorf("name = %q", prop.Name)
	}
	if prop.Namespace != "observability" {
		t.Errorf("namespace = %q", prop.Namespace)
	}
	if prop.Spec.ProposedEventType != "agent.redis.failover.proposed" {
		t.Errorf("type = %q", prop.Spec.ProposedEventType)
	}
	if prop.Spec.Action != "failover" {
		t.Errorf("action = %q", prop.Spec.Action)
	}
	if prop.Spec.CorrelationID != "01HZ8W000000000000000000" {
		t.Errorf("correlationId = %q", prop.Spec.CorrelationID)
	}
	want := time.Date(2026, 5, 15, 19, 30, 0, 0, time.UTC)
	if !prop.Spec.ExpiresAt.Time.Equal(want) {
		t.Errorf("expiresAt = %v, want %v", prop.Spec.ExpiresAt.Time, want)
	}
	if prop.Spec.Plan == nil || prop.Spec.Plan.Body != `{"newPrimary": "replica-2"}` {
		t.Errorf("plan body = %q", prop.Spec.Plan.Body)
	}
	if prop.Spec.Evidence == nil || len(prop.Spec.Evidence.CorrelationIDs) != 1 {
		t.Errorf("evidence = %+v", prop.Spec.Evidence)
	}
}

func TestProposalFromCE_Rejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not-json", `not json`},
		{"wrong-type-suffix", swap(validCE, ".proposed", ".info")},
		{"wrong-type-prefix", swap(validCE, "agent.redis.", "kratix.redis.")},
		{"missing-proposal-id", swap(validCE, `"proposalId": "01HZ9A0000000000000000000",`, "")},
		{"missing-action", swap(validCE, `"action": "failover",`, "")},
		{"missing-rationale", swap(validCE, `"rationale": "3 lag spikes > 30s in last 10m",`, "")},
		{"missing-expiresAt", swap(validCE, `"expiresAt": "2026-05-15T19:30:00Z",`, "")},
		{"unparseable-expiresAt", swap(validCE, `"2026-05-15T19:30:00Z"`, `"tomorrow"`)},
		{"empty-data", swap(validCE, `"data": {`, `"data": null, "_": {`)}, // makes data null but keeps the rest parseable
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := proposalFromCE([]byte(tc.body), "observability")
			if err == nil {
				t.Fatal("expected error")
			}
			if !isReject(err) {
				t.Errorf("expected errReject, got %T: %v", err, err)
			}
		})
	}
}

func TestSwapSuffix(t *testing.T) {
	cases := map[string]string{
		"agent.redis.failover.proposed":  "agent.redis.failover.approved",
		"agent.x.y.proposed":             "agent.x.y.approved",
		"agent.x.y.executed":             "agent.x.y.executed", // not .proposed; unchanged
	}
	for in, want := range cases {
		if got := approvedTypeForProposed(in); got != want {
			t.Errorf("approvedTypeForProposed(%q) = %q, want %q", in, got, want)
		}
	}
	if got := expiredTypeForProposed("agent.x.y.proposed"); got != "agent.x.y.expired" {
		t.Errorf("expiredTypeForProposed: %q", got)
	}
}

// swap is a tiny helper to mutate the test fixture string.
func swap(s, old, new string) string {
	// Use a simple string replace; the test fixtures contain unique substrings.
	out := []byte(s)
	idx := indexOf(out, []byte(old))
	if idx < 0 {
		return s
	}
	return string(out[:idx]) + new + string(out[idx+len(old):])
}

func indexOf(haystack, needle []byte) int {
loop:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue loop
			}
		}
		return i
	}
	return -1
}
