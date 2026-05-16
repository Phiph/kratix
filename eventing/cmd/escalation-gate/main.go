// Command escalation-gate is the controller for AgentProposal CRs.
//
// It has two responsibilities:
//
//  1. Receive .proposed CloudEvents over HTTP (routed by a CloudEventSink
//     with typeFilter=agent.*.proposed). For each event, materialise an
//     AgentProposal CR in the configured namespace. Idempotent on the
//     payload's proposalId.
//
//  2. Watch AgentProposal CRs in that namespace and drive them through
//     their state machine: when the agents.kratix.io/approved-by
//     annotation appears, emit the matching .approved CloudEvent and
//     mark the CR resolved; when spec.expiresAt passes without approval,
//     emit .expired and mark the CR resolved.
//
// v0.1 is single-replica and uses a polling watcher rather than an
// informer-driven controller. The expected proposal rate is human-paced;
// the simplicity is worth more than the latency.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(eventingv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		kubeconfig string
		namespace  string
		listenAddr string
		sweep      time.Duration
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig; empty for in-cluster")
	flag.StringVar(&namespace, "namespace", "kratix-platform-system", "namespace for AgentProposal CRs")
	flag.StringVar(&listenAddr, "listen", ":8080", "HTTP listen address")
	flag.DurationVar(&sweep, "sweep-interval", 15*time.Second, "how often to scan for approvals + expiry")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		log.Error("kubeconfig", "err", err)
		os.Exit(1)
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("controller-runtime client", "err", err)
		os.Exit(1)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("kubernetes client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	rcv := newReceiver(cli, namespace, log)
	wch := newWatcher(cli, kc, namespace, sweep, log)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           rcv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("escalation-gate listening", "addr", listenAddr, "namespace", namespace)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			cancel()
		}
	}()
	go wch.Run(ctx)

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

// loadKubeconfig resolves a *rest.Config from an explicit path, the
// KUBECONFIG env var, or in-cluster — in that order. Avoids importing
// controller-runtime's package-level init() which registers a global
// --kubeconfig flag and would collide with this binary's own flag set.
func loadKubeconfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return clientcmd.BuildConfigFromFlags("", env)
	}
	return rest.InClusterConfig()
}
