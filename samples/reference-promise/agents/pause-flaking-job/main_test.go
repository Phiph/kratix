package main

import "testing"

func TestExtractScheduledJob_PrefersPayload(t *testing.T) {
	// Job-attributed subject; payload has the parent ScheduledJob name.
	subject := "team-a/job/scheduled-job-nightly-cleanup-1700000000"
	data := []byte(`{"scheduledJob": "nightly-cleanup", "jobName": "scheduled-job-nightly-cleanup-1700000000", "retryCount": 3}`)
	name, ns := extractScheduledJob(data, subject)
	if name != "nightly-cleanup" {
		t.Errorf("name = %q; want nightly-cleanup", name)
	}
	if ns != "team-a" {
		t.Errorf("namespace = %q; want team-a", ns)
	}
}

func TestExtractScheduledJob_MissingPayload(t *testing.T) {
	subject := "team-a/job/scheduled-job-nightly-cleanup-1700000000"
	name, ns := extractScheduledJob(nil, subject)
	if name != "" {
		t.Errorf("name = %q; expected empty without payload", name)
	}
	if ns != "team-a" {
		t.Errorf("namespace = %q; namespace should still parse from subject", ns)
	}
}

func TestExtractScheduledJob_MalformedSubject(t *testing.T) {
	name, ns := extractScheduledJob([]byte(`{"scheduledJob":"foo"}`), "not-a-subject")
	if name != "foo" {
		t.Errorf("name = %q", name)
	}
	if ns != "" {
		t.Errorf("namespace = %q; expected empty for malformed subject", ns)
	}
}

func TestExtractProposalID(t *testing.T) {
	cases := map[string]string{
		`{"proposalId":"psj-abc"}`: "psj-abc",
		`{}`:                        "",
		``:                          "",
		`not-json`:                  "",
	}
	for in, want := range cases {
		if got := extractProposalID([]byte(in)); got != want {
			t.Errorf("extractProposalID(%q) = %q, want %q", in, got, want)
		}
	}
}
