package main

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newJob(name string, opts ...func(*batchv1.Job)) *batchv1.Job {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "team-a",
			Labels: map[string]string{
				"app.kubernetes.io/instance":  "nightly-cleanup",
				"platform.kratix.io/owned-by": "scheduled-job-promise",
			},
			OwnerReferences: []metav1.OwnerReference{
				{Kind: "CronJob", Name: "scheduled-job-nightly-cleanup"},
			},
		},
	}
	for _, opt := range opts {
		opt(j)
	}
	return j
}

func withActive(n int32) func(*batchv1.Job) {
	return func(j *batchv1.Job) { j.Status.Active = n }
}
func withSucceeded(n int32) func(*batchv1.Job) {
	return func(j *batchv1.Job) { j.Status.Succeeded = n }
}
func withFailed(n int32) func(*batchv1.Job) {
	return func(j *batchv1.Job) { j.Status.Failed = n }
}
func withTimes(start, end time.Time) func(*batchv1.Job) {
	return func(j *batchv1.Job) {
		s := metav1.NewTime(start)
		j.Status.StartTime = &s
		e := metav1.NewTime(end)
		j.Status.CompletionTime = &e
	}
}

func TestClassifyJob_StartedFromNoPrev(t *testing.T) {
	curr := newJob("nightly-cleanup-1700000000", withActive(1))
	tr := classifyJob(nil, curr)
	if tr == nil {
		t.Fatal("expected transition")
	}
	if tr.Type != "kratix.scheduledjob.job.started" {
		t.Errorf("type = %q", tr.Type)
	}
	if tr.Severity != "info" {
		t.Errorf("severity = %q", tr.Severity)
	}
	if tr.ScheduledJob != "nightly-cleanup" {
		t.Errorf("scheduledJob = %q", tr.ScheduledJob)
	}
}

func TestClassifyJob_Completed(t *testing.T) {
	start := time.Date(2026, 5, 15, 19, 0, 0, 0, time.UTC)
	curr := newJob("j1", withSucceeded(1), withTimes(start, start.Add(47*time.Second)))
	tr := classifyJob(nil, curr)
	if tr == nil || tr.Type != "kratix.scheduledjob.job.completed" {
		t.Fatalf("expected completed, got %+v", tr)
	}
	if tr.Data["durationSeconds"] != "47" {
		t.Errorf("durationSeconds = %q", tr.Data["durationSeconds"])
	}
}

func TestClassifyJob_Failed_OnlyEmitsWhenRetriesExhausted(t *testing.T) {
	// Job is still retrying (Active > 0): no emission yet.
	mid := newJob("j1", withFailed(1), withActive(1))
	if tr := classifyJob(nil, mid); tr != nil {
		t.Errorf("did not expect emission while retries pending: %+v", tr)
	}
	// Retries exhausted (Active == 0): emit failure.
	done := newJob("j1", withFailed(3), withActive(0))
	tr := classifyJob(nil, done)
	if tr == nil || tr.Type != "kratix.scheduledjob.job.failed" {
		t.Fatalf("expected failed, got %+v", tr)
	}
	if tr.Severity != "warning" {
		t.Errorf("severity = %q", tr.Severity)
	}
	if tr.Data["retryCount"] != "3" {
		t.Errorf("retryCount = %q", tr.Data["retryCount"])
	}
}

func TestClassifyJob_NoEmissionIfNotOwned(t *testing.T) {
	curr := newJob("foreign", withActive(1))
	delete(curr.Labels, "platform.kratix.io/owned-by")
	if tr := classifyJob(nil, curr); tr != nil {
		t.Errorf("expected no emission for non-promise-owned Job, got %+v", tr)
	}
}

func TestClassifyJob_NoDuplicateOnAlreadySeen(t *testing.T) {
	prev := newJob("j1", withSucceeded(1))
	curr := newJob("j1", withSucceeded(1))
	if tr := classifyJob(prev, curr); tr != nil {
		t.Errorf("expected no duplicate emission, got %+v", tr)
	}
}

func TestClassifyCronJobSuspended_OnlyOnFlip(t *testing.T) {
	if tr := classifyCronJobSuspended(false, true, "nightly-cleanup", "scheduled-job-nightly-cleanup"); tr == nil {
		t.Fatal("expected transition on false→true flip")
	}
	if tr := classifyCronJobSuspended(true, true, "nightly-cleanup", "scheduled-job-nightly-cleanup"); tr != nil {
		t.Errorf("did not expect re-emit while already suspended: %+v", tr)
	}
	if tr := classifyCronJobSuspended(false, false, "nightly-cleanup", "scheduled-job-nightly-cleanup"); tr != nil {
		t.Errorf("did not expect transition for unsuspended → unsuspended: %+v", tr)
	}
}

func TestReasonFor(t *testing.T) {
	cases := map[string]string{
		"kratix.scheduledjob.job.failed":    "ScheduledjobJobFailed",
		"kratix.scheduledjob.job.completed": "ScheduledjobJobCompleted",
		"agent.scheduledjob.pause.proposed": "ScheduledjobPauseProposed",
		"singleSegment":                     "SingleSegment",
	}
	for in, want := range cases {
		if got := reasonFor(in); got != want {
			t.Errorf("reasonFor(%q) = %q, want %q", in, got, want)
		}
	}
}
