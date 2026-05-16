// Command pause-flaking-job is an action-taking agent for the
// ScheduledJob Promise. It illustrates the full escalation-gate loop on
// a real, low-blast-radius action: suspending a CronJob.
//
// Wire:
//
//	(1) forwarder POSTs kratix.scheduledjob.job.failed CEs to /events.
//	(2) agent counts failures per ScheduledJob subject in a rolling window.
//	(3) on threshold, agent emits agent.scheduledjob.pause.proposed.
//	(4) gate controller materialises an AgentProposal CR.
//	(5) approver runs kratix-approve.
//	(6) gate emits agent.scheduledjob.pause.approved.
//	(7) forwarder POSTs the .approved CE back to /events.
//	(8) agent matches it against its pending proposals + patches the
//	    ScheduledJob spec.suspended=true.
//	(9) agent emits agent.scheduledjob.pause.executed.
//
// State is in-memory:
//   - The failure window (window.go).
//   - A pending-proposals map: proposalId → (scheduledJob, namespace).
//
// A restart loses both. Acceptable for v0.1 because:
//   - Restart-and-re-observe rebuilds the window naturally.
//   - In-flight proposals that lose the agent will resolve at the gate as
//     .expired; the resulting .executed never fires; the proposing-and-
//     forgetting agent eventually re-proposes if the pattern persists.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// inboundEvent is the subset of the forwarded CE the agent reads.
type inboundEvent struct {
	Type          string          `json:"type"`
	Subject       string          `json:"subject"`
	CorrelationID string          `json:"kratixcorrelationid"`
	Data          json.RawMessage `json:"data"`
}

type pendingProposal struct {
	scheduledJob      string
	scheduledJobNamespace string
	correlationID     string
	createdAt         time.Time
}

type agent struct {
	log       *slog.Logger
	window    *failureWindow
	proposer  *proposer
	executor  *executor

	threshold       int
	horizonMinutes  int

	mu      sync.Mutex
	pending map[string]pendingProposal // proposalId → context

	now func() time.Time
}

func main() {
	var (
		kubeconfig       string
		listen           string
		emitNamespace    string
		proposalNS       string
		failureThreshold int
		windowHorizon    time.Duration
		expiry           time.Duration
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig; empty for in-cluster or KUBECONFIG env")
	flag.StringVar(&listen, "listen", ":8080", "HTTP listen address")
	flag.StringVar(&emitNamespace, "emit-namespace", "kratix-platform-system", "namespace where this agent writes emitted Events")
	flag.StringVar(&proposalNS, "proposal-namespace", "kratix-platform-system", "namespace where the gate controller materialises AgentProposals")
	flag.IntVar(&failureThreshold, "failure-threshold", 3, "failures within the window required to propose pausing")
	flag.DurationVar(&windowHorizon, "window-horizon", 30*time.Minute, "rolling window for counting failures")
	flag.DurationVar(&expiry, "proposal-expiry", 15*time.Minute, "how long humans have to approve a proposal")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		log.Error("kubeconfig", "err", err)
		os.Exit(1)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		log.Error("dynamic client", "err", err)
		os.Exit(1)
	}

	hostname, _ := os.Hostname()
	actor := "agent/pause-flaking-job/v0.1/" + hostname

	a := &agent{
		log:            log,
		window:         newFailureWindow(windowHorizon, failureThreshold),
		proposer:       newProposer(kc, emitNamespace, proposalNS, actor, expiry),
		executor:       newExecutor(dc, kc, emitNamespace, actor),
		threshold:      failureThreshold,
		horizonMinutes: int(windowHorizon.Minutes()),
		pending:        map[string]pendingProposal{},
		now:            time.Now,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/events", a.handleEvent)

	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("pause-flaking-job listening", "addr", listen, "threshold", failureThreshold, "horizon", windowHorizon.String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

func (a *agent) handleEvent(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer req.Body.Close()
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var ce inboundEvent
	if err := json.Unmarshal(body, &ce); err != nil {
		a.log.Warn("malformed inbound event", "err", err)
		// Ack so the forwarder doesn't retry; the upstream emitter is buggy
		// but that's not for us to fix here.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if ce.Type == "" || ce.Subject == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch {
	case ce.Type == "kratix.scheduledjob.job.failed":
		a.onFailure(req.Context(), ce)
	case ce.Type == "kratix.scheduledjob.job.completed":
		// Successful run; reset the agent's flakiness view.
		a.window.Clear(ce.Subject)
	case ce.Type == "agent.scheduledjob.pause.approved":
		a.onApproved(req.Context(), ce)
	case ce.Type == "agent.scheduledjob.pause.expired":
		a.onExpired(ce)
	default:
		// Subscribed-but-uninteresting; ack and move on.
	}
	w.WriteHeader(http.StatusAccepted)
}

func (a *agent) onFailure(ctx context.Context, ce inboundEvent) {
	// The job-watcher attributes failures to specific Jobs (Job names are
	// per-run, e.g. "scheduled-job-nightly-cleanup-1700000000"). We want
	// to count *per ScheduledJob*, not per Job. The payload carries the
	// authoritative scheduledJob field — use it as the window key.
	scheduledJobName, scheduledJobNamespace := extractScheduledJob(ce.Data, ce.Subject)
	if scheduledJobName == "" {
		a.log.Warn("could not extract scheduledJob from failure event", "subject", ce.Subject)
		return
	}
	key := scheduledJobNamespace + "/scheduledjob/" + scheduledJobName

	a.window.Observe(key)
	count, tripped := a.window.Trips(key)
	if !tripped {
		a.log.Debug("failure observed; below threshold", "scheduledJob", key, "count", count, "threshold", a.threshold)
		return
	}

	// Don't pile on: if we already have a pending proposal for this subject,
	// skip. The previous proposal will resolve one way or another.
	if a.hasPendingForSubject(scheduledJobName, scheduledJobNamespace) {
		a.log.Debug("threshold tripped but proposal already pending", "scheduledJob", key)
		return
	}

	proposalID, err := a.proposer.Propose(ctx, key, scheduledJobName, scheduledJobNamespace, count, a.horizonMinutes)
	if err != nil {
		a.log.Warn("propose failed", "err", err, "subject", ce.Subject)
		return
	}
	a.mu.Lock()
	a.pending[proposalID] = pendingProposal{
		scheduledJob:          scheduledJobName,
		scheduledJobNamespace: scheduledJobNamespace,
		correlationID:         ce.CorrelationID,
		createdAt:             a.now(),
	}
	a.mu.Unlock()
	a.log.Info("proposed pause", "scheduledJob", scheduledJobNamespace+"/"+scheduledJobName, "proposalId", proposalID, "count", count)
}

func (a *agent) onApproved(ctx context.Context, ce inboundEvent) {
	proposalID := extractProposalID(ce.Data)
	if proposalID == "" {
		return
	}
	a.mu.Lock()
	pending, ok := a.pending[proposalID]
	if ok {
		delete(a.pending, proposalID)
	}
	a.mu.Unlock()
	if !ok {
		// Not ours — could be a different replica's proposal or a stale
		// echo. Ignore.
		return
	}
	if err := a.executor.Execute(ctx, proposalID, pending.scheduledJob, pending.scheduledJobNamespace, pending.correlationID); err != nil {
		a.log.Warn("execute failed", "err", err, "proposalId", proposalID)
		return
	}
	a.log.Info("executed pause", "proposalId", proposalID, "scheduledJob", pending.scheduledJob)
}

func (a *agent) onExpired(ce inboundEvent) {
	proposalID := extractProposalID(ce.Data)
	if proposalID == "" {
		return
	}
	a.mu.Lock()
	delete(a.pending, proposalID)
	a.mu.Unlock()
	a.log.Info("proposal expired", "proposalId", proposalID, "subject", ce.Subject)
}

func (a *agent) hasPendingForSubject(name, ns string) bool {
	if name == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.pending {
		if p.scheduledJob == name && p.scheduledJobNamespace == ns {
			return true
		}
	}
	return false
}

// extractScheduledJob pulls the (name, namespace) of the parent
// ScheduledJob from a job.failed CE. The job-watcher embeds it in the
// payload (`scheduledJob` field); the involvedObject's namespace gives
// us the cluster location. Falling back to subject-parsing is only a
// last-resort hint if the payload is malformed.
func extractScheduledJob(data json.RawMessage, subject string) (name, namespace string) {
	var p struct {
		ScheduledJob string `json:"scheduledJob"`
	}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &p)
	}
	// Subject is "<ns>/<kind>/<name>". The namespace is part 0; the kind
	// (Job) sits in part 1 and is not what we want.
	parts := strings.Split(subject, "/")
	if len(parts) == 3 {
		namespace = parts[0]
	}
	if p.ScheduledJob != "" {
		return p.ScheduledJob, namespace
	}
	return "", namespace
}

func extractProposalID(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var payload struct {
		ProposalID string `json:"proposalId"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.ProposalID
}

func loadKubeconfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return clientcmd.BuildConfigFromFlags("", env)
	}
	return rest.InClusterConfig()
}
