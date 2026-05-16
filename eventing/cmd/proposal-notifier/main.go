// Command proposal-notifier is the reference notifier for the escalation gate.
//
// It hosts an HTTP endpoint that the kratix-event-forwarder routes
// agent.*.proposed CloudEvents to (via a CloudEventSink). For each event
// it transforms the proposal into either a generic JSON envelope or a
// Slack Block Kit payload, then POSTs the result to a configured target.
//
// The notifier is deliberately Slack/Teams/PagerDuty-agnostic at the wire
// layer. To target a particular tool, set --target to that tool's
// incoming-webhook URL. The two built-in formats cover the common cases:
//
//	--format=generic  (default) → arbitrary tool that accepts JSON POSTs
//	--format=slack              → Slack incoming webhook (Block Kit)
//
// To plumb in a different formatter (Teams Adaptive Cards, PagerDuty
// Events v2), add a small Go file that takes genericEnvelope and writes
// the target-native shape.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const defaultListen = ":8080"

func main() {
	var (
		listen    string
		target    string
		format    string
		namespace string
		timeout   time.Duration
	)
	flag.StringVar(&listen, "listen", defaultListen, "HTTP listen address")
	flag.StringVar(&target, "target", "", "URL to POST notifications to (e.g. a Slack incoming webhook). Required.")
	flag.StringVar(&format, "format", "generic", "output format: 'generic' | 'slack'")
	flag.StringVar(&namespace, "proposal-namespace", "kratix-platform-system", "namespace where AgentProposals live (used in approve hints)")
	flag.DurationVar(&timeout, "post-timeout", 5*time.Second, "per-POST HTTP timeout")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if target == "" {
		log.Error("--target is required")
		os.Exit(2)
	}
	if format != "generic" && format != "slack" {
		log.Error("--format must be 'generic' or 'slack'", "got", format)
		os.Exit(2)
	}

	relay := &relay{
		client:    &http.Client{Timeout: timeout},
		target:    target,
		format:    format,
		namespace: namespace,
		now:       time.Now,
		log:       log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/events", relay.handle)

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Info("proposal-notifier listening", "addr", listen, "format", format)
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

// relay holds the wiring between the inbound CE receiver and the outbound
// notification target. Kept as a struct so handle is testable without
// touching globals.
type relay struct {
	client    *http.Client
	target    string
	format    string
	namespace string
	now       func() time.Time
	log       *slog.Logger
}

func (r *relay) handle(w http.ResponseWriter, req *http.Request) {
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

	env, err := transform(body, r.namespace, r.now())
	if err != nil {
		if isReject(err) {
			r.log.Debug("dropped event", "err", err)
			w.WriteHeader(http.StatusAccepted) // ack so the forwarder doesn't retry
			return
		}
		r.log.Error("transform failed", "err", err)
		http.Error(w, "transform failed", http.StatusInternalServerError)
		return
	}

	payload, err := r.encode(env)
	if err != nil {
		r.log.Error("encode failed", "err", err, "format", r.format)
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}

	if err := r.post(req.Context(), payload); err != nil {
		r.log.Warn("post to target failed", "err", err, "target", r.target)
		// Don't 500 — the forwarder would retry, but the upstream agent
		// has already done its job. Drop with a counter (TODO) and ack.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (r *relay) encode(env genericEnvelope) ([]byte, error) {
	switch r.format {
	case "slack":
		return json.Marshal(toSlack(env))
	default:
		return json.Marshal(env)
	}
}

func (r *relay) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain the body to satisfy keep-alive, but include a hint in the
		// error so debugging Slack's enthusiastic 4xx responses is easier.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("target status %d: %s", resp.StatusCode, string(snippet))
	}
	return nil
}
