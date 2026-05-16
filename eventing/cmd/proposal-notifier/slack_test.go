package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToSlack_RoundTripsAsJSON(t *testing.T) {
	env := genericEnvelope{
		Action:     "failover",
		Subject:    "default/promise/redis",
		Actor:      "agent/x/v1",
		ExpiresIn:  "15m0s",
		Rationale:  "lag spikes",
		ProposalID: "01HZ",
		ApproveCmd: "kratix-approve 01HZ ...",
	}
	out, err := json.Marshal(toSlack(env))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check the load-bearing pieces appear; we don't pin block IDs.
	s := string(out)
	for _, want := range []string{
		`"blocks"`,
		`"header"`,
		`"section"`,
		`"context"`,
		`failover`,
		`default/promise/redis`,
		`kratix-approve 01HZ ...`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in Slack payload: %s", want, s)
		}
	}
}
