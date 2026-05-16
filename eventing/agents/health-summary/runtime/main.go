// Command health-summary-agent is the long-running runtime for the
// HealthSummaryAgent Promise.
//
// It accepts CloudEvent POSTs on /events from the kratix-event-forwarder,
// maintains a fixed 1h rolling window keyed by CloudEvent subject, and on
// a configured cadence re-emits a digest as its own CloudEvent (via a
// kratix.io/ce-* annotated Kubernetes Event, picked up by the forwarder).
//
// Configuration is via env vars set by the pipeline-produced ConfigMap:
//
//	digestSchedule  — "@hourly" | "@daily" | "@every <duration>"
//	subscribe       — comma-separated list (informational; the
//	                  CloudEventSink does the actual filtering)
//	AGENT_INSTANCE  — name of the HealthSummaryAgent CR
//	AGENT_NAMESPACE — namespace of the HealthSummaryAgent CR
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	listenAddr    = ":8080"
	windowHorizon = time.Hour
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	schedule := envOr("digestSchedule", "@hourly")
	instance := mustEnv("AGENT_INSTANCE", log)
	namespace := mustEnv("AGENT_NAMESPACE", log)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Error("in-cluster config", "err", err)
		os.Exit(1)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}

	w := newWindow(windowHorizon)
	r := newReceiver(w, log)
	d, err := newDigester(w, kc, namespace, instance, schedule, log)
	if err != nil {
		log.Error("digester init", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           r.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("receiver listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "err", err)
			cancel()
		}
	}()

	go d.Run(ctx)

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func mustEnv(key string, log *slog.Logger) string {
	v := os.Getenv(key)
	if v == "" {
		log.Error("required env var unset", "key", key)
		os.Exit(2)
	}
	return v
}
