package main

import (
	"testing"
	"time"
)

const validProposed = `{
  "specversion": "1.0",
  "type": "agent.redis.failover.proposed",
  "subject": "default/promise/redis",
  "time": "2026-05-15T19:00:00Z",
  "kratixcorrelationid": "01HZ-CORR",
  "data": {
    "action": "failover",
    "actor": "agent/redis-flake-detector/v1.2.0",
    "subject": "default/promise/redis",
    "rationale": "3 lag spikes > 30s",
    "proposalId": "01HZ-PROP",
    "expiresAt": "2026-05-15T19:15:00Z"
  }
}`

func TestTransform_Happy(t *testing.T) {
	now := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	env, err := transform([]byte(validProposed), "kratix-platform-system", now)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if env.Event != "agent.redis.failover.proposed" {
		t.Errorf("event = %q", env.Event)
	}
	if env.ProposalID != "01HZ-PROP" {
		t.Errorf("proposalId = %q", env.ProposalID)
	}
	if env.Namespace != "kratix-platform-system" {
		t.Errorf("namespace = %q", env.Namespace)
	}
	if env.ExpiresIn != "15m0s" {
		t.Errorf("expiresIn = %q (now+15m → want 15m0s)", env.ExpiresIn)
	}
	if env.ApproveCmd == "" {
		t.Error("approveCmd unset")
	}
	if env.KubectlCmd == "" {
		t.Error("kubectlCmd unset")
	}
}

func TestTransform_RejectsNonProposed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not-json", `nope`},
		{"non-agent-type", swap(validProposed, "agent.redis.failover.proposed", "kratix.promise.unavailable")},
		{"non-proposed-suffix", swap(validProposed, ".proposed", ".executed")},
		{"missing-proposalId", swap(validProposed, `"proposalId": "01HZ-PROP",`, "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := transform([]byte(tc.body), "ns", time.Now())
			if err == nil {
				t.Fatal("expected reject")
			}
			if !isReject(err) {
				t.Fatalf("got non-reject error: %v", err)
			}
		})
	}
}

func TestTransform_ExpiredShowsAgo(t *testing.T) {
	// Proposal expired 5 minutes ago.
	now := time.Date(2026, 5, 15, 19, 20, 0, 0, time.UTC)
	env, err := transform([]byte(validProposed), "ns", now)
	if err != nil {
		t.Fatal(err)
	}
	if env.ExpiresIn != "expired 5m0s ago" {
		t.Errorf("expiresIn = %q, want 'expired 5m0s ago'", env.ExpiresIn)
	}
}

func swap(s, old, new string) string {
	idx := indexOf([]byte(s), []byte(old))
	if idx < 0 {
		return s
	}
	return s[:idx] + new + s[idx+len(old):]
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
