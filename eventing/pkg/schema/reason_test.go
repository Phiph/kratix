package schema

import "testing"

func TestReasonToType(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"PromiseUnavailable", "kratix.promise.unavailable", true},
		{"PromiseAvailable", "kratix.promise.available", true},
		{"PromiseReady", "kratix.promise.ready", true},
		{"WorkPlacementWriteFailed", "kratix.work.placement.write.failed", true},
		{"P", "kratix.p", true},
		{"", "", false},
		{"lowercaseStart", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := ReasonToType(tc.in)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ReasonToType(%q) = (%q, %v), want (%q, %v)",
					tc.in, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestSeverityFromEventType(t *testing.T) {
	cases := map[string]string{
		"Normal":  SeverityInfo,
		"Warning": SeverityWarning,
		"":        SeverityInfo,
		"Other":   SeverityInfo,
	}
	for in, want := range cases {
		if got := SeverityFromEventType(in); got != want {
			t.Errorf("SeverityFromEventType(%q) = %q, want %q", in, got, want)
		}
	}
}
