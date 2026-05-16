package main

import (
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

// transition describes a Job state transition the observer wants to emit.
// Pure data — the wiring layer turns this into an annotated K8s Event.
type transition struct {
	Type           string            // CloudEvent type, e.g. kratix.scheduledjob.job.failed
	Severity       string            // info | warning
	ScheduledJob   string            // value of the platform.kratix.io/instance label
	JobName        string
	CronJobName    string
	Reason         string            // PascalCase (Event.reason)
	Message        string
	Data           map[string]string // serialised into kratix.io/ce-data
}

// reasonFor returns the kratix-shaped Event.reason for a CE type.
// Mirrors schema.ReasonToType semantics in reverse — kratix.X.Y.Z → XYZ
// in PascalCase. Inlined here rather than imported from eventing/pkg/schema
// to keep this binary's import footprint tiny.
func reasonFor(ceType string) string {
	// Strip "kratix." or "agent." prefix.
	for _, p := range []string{"kratix.", "agent."} {
		if len(ceType) > len(p) && ceType[:len(p)] == p {
			ceType = ceType[len(p):]
			break
		}
	}
	var out []byte
	upper := true
	for i := 0; i < len(ceType); i++ {
		c := ceType[i]
		if c == '.' {
			upper = true
			continue
		}
		if upper && c >= 'a' && c <= 'z' {
			c -= 32
		}
		upper = false
		out = append(out, c)
	}
	return string(out)
}

// classifyJob inspects a Job and returns the *new* transition relative to
// the previous observation, or nil if nothing emitable changed.
//
// The classification is deliberately simple:
//
//	prev=nil + Active>0                              → started
//	Succeeded>0 and prev did not see Succeeded       → completed
//	Failed>0   and prev did not see Failed           → failed
//	Suspended (parent CronJob) flips to true         → skipped
//
// The CronJob suspension transition is handled by the wiring layer (which
// watches CronJobs separately); this function operates only on Jobs.
func classifyJob(prev, curr *batchv1.Job) *transition {
	if curr == nil {
		return nil
	}
	scheduled := scheduledJobLabel(curr)
	if scheduled == "" {
		// Not one of ours.
		return nil
	}
	cronName := cronJobLabel(curr)

	// Started: a Job goes Active for the first time and hasn't failed or
	// succeeded yet. Without the Failed/Succeeded==0 guard, an observer
	// joining mid-retry would emit a false "started" — Active>0 is true
	// for both the initial pod and the backoff-relaunched pod.
	if prev == nil && curr.Status.Active > 0 &&
		curr.Status.Failed == 0 && curr.Status.Succeeded == 0 {
		return &transition{
			Type:         "kratix.scheduledjob.job.started",
			Severity:     "info",
			ScheduledJob: scheduled,
			JobName:      curr.Name,
			CronJobName:  cronName,
			Reason:       reasonFor("kratix.scheduledjob.job.started"),
			Message:      fmt.Sprintf("ScheduledJob %s: Job %s started", scheduled, curr.Name),
			Data: map[string]string{
				"scheduledJob": scheduled,
				"cronJobName":  cronName,
				"jobName":      curr.Name,
			},
		}
	}

	// Completed: Succeeded transitioned 0 → >0.
	if curr.Status.Succeeded > 0 && (prev == nil || prev.Status.Succeeded == 0) {
		dur := jobDuration(curr)
		return &transition{
			Type:         "kratix.scheduledjob.job.completed",
			Severity:     "info",
			ScheduledJob: scheduled,
			JobName:      curr.Name,
			CronJobName:  cronName,
			Reason:       reasonFor("kratix.scheduledjob.job.completed"),
			Message:      fmt.Sprintf("ScheduledJob %s: Job %s completed in %s", scheduled, curr.Name, dur),
			Data: map[string]string{
				"scheduledJob":    scheduled,
				"jobName":         curr.Name,
				"durationSeconds": strconv.FormatInt(int64(dur.Seconds()), 10),
			},
		}
	}

	// Failed: Failed transitioned 0 → >0 *and* the Job has no more retries
	// queued (Active==0). This avoids emitting on every backoff retry.
	if curr.Status.Failed > 0 && curr.Status.Active == 0 &&
		(prev == nil || prev.Status.Failed == 0 || prev.Status.Active > 0) {
		retries := int64(curr.Status.Failed)
		return &transition{
			Type:         "kratix.scheduledjob.job.failed",
			Severity:     "warning",
			ScheduledJob: scheduled,
			JobName:      curr.Name,
			CronJobName:  cronName,
			Reason:       reasonFor("kratix.scheduledjob.job.failed"),
			Message:      fmt.Sprintf("ScheduledJob %s: Job %s failed after %d attempts", scheduled, curr.Name, retries),
			Data: map[string]string{
				"scheduledJob": scheduled,
				"jobName":      curr.Name,
				"retryCount":   strconv.FormatInt(retries, 10),
			},
		}
	}

	return nil
}

// classifyCronJobSuspended returns a transition for a CronJob whose
// suspend status flipped to true since the last observation. The
// `Skipped` event is emitted once per flip, not once per missed tick —
// every tick while suspended would spam the bus.
func classifyCronJobSuspended(prevSuspended, currSuspended bool, scheduledJob, cronName string) *transition {
	if scheduledJob == "" || !currSuspended || prevSuspended {
		return nil
	}
	return &transition{
		Type:         "kratix.scheduledjob.job.skipped",
		Severity:     "info",
		ScheduledJob: scheduledJob,
		CronJobName:  cronName,
		Reason:       reasonFor("kratix.scheduledjob.job.skipped"),
		Message:      fmt.Sprintf("ScheduledJob %s suspended; further ticks will be skipped", scheduledJob),
		Data: map[string]string{
			"scheduledJob": scheduledJob,
		},
	}
}

func scheduledJobLabel(j *batchv1.Job) string {
	// First try our promise label (set by the pipeline via the CronJob's
	// jobTemplate labels). Fall back to the CronJob owner reference name.
	if v := j.Labels["app.kubernetes.io/instance"]; v != "" {
		// Only emit for objects owned by our promise; otherwise we'd
		// happily emit events about every CronJob in the cluster.
		if owned := j.Labels["platform.kratix.io/owned-by"]; owned != "scheduled-job-promise" {
			return ""
		}
		return v
	}
	return ""
}

func cronJobLabel(j *batchv1.Job) string {
	for _, ref := range j.OwnerReferences {
		if ref.Kind == "CronJob" {
			return ref.Name
		}
	}
	return ""
}

func jobDuration(j *batchv1.Job) time.Duration {
	if j.Status.StartTime == nil || j.Status.CompletionTime == nil {
		return 0
	}
	d := j.Status.CompletionTime.Sub(j.Status.StartTime.Time)
	if d < 0 {
		return 0
	}
	return d
}
