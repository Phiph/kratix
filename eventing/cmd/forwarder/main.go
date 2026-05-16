// Command forwarder watches Kubernetes Event objects in the cluster, filters
// for Kratix-namespaced events (per eventing/WIRE-FORMAT.md), translates them
// into CloudEvents v1.0 envelopes, and POSTs them to every CloudEventSink CR
// whose typeFilter matches.
//
// v0.1 scope:
//   - Synchronous fan-out, no retries beyond per-request timeout.
//   - Drops malformed Events with a log line; no dead-letter queue.
//   - No leader election (single replica only for MVP).
//   - No status writes back to CloudEventSink (Conditions are present in the
//     CRD but unmanaged in v0.1).
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
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
		installID  string
		resync     time.Duration
		httpTO     time.Duration
		logLevel   string
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig; empty for in-cluster config")
	flag.StringVar(&installID, "install-id", "", "identifier for this Kratix install; surfaces as kratixinstallid extension")
	flag.DurationVar(&resync, "resync", 10*time.Minute, "informer resync period")
	flag.DurationVar(&httpTO, "http-timeout", 5*time.Second, "per-sink HTTP request timeout")
	flag.StringVar(&logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	log := newLogger(logLevel)
	if installID == "" {
		log.Error("--install-id is required")
		os.Exit(2)
	}

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		log.Error("load kubeconfig", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, cfg, installID, resync, httpTO, log); err != nil {
		log.Error("forwarder exited with error", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg *rest.Config, installID string, resync, httpTO time.Duration, log *slog.Logger) error {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	store := newSinkStore()
	if err := bootstrapSinks(ctx, cli, store); err != nil {
		return err
	}

	deliv := newDeliverer(httpTO, log)

	factory := informers.NewSharedInformerFactory(kc, resync)
	eventInformer := factory.Core().V1().Events().Informer()
	_, err = eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			ev, ok := obj.(*corev1.Event)
			if !ok {
				return
			}
			handleEvent(ctx, ev, installID, store, deliv, log)
		},
	})
	if err != nil {
		return err
	}

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	// Lightweight poll of CloudEventSinks: avoids dragging in controller-runtime
	// manager machinery for v0.1. A ticker every resync interval is plenty —
	// sink config changes are not latency-sensitive.
	go pollSinks(ctx, cli, store, resync, log)

	log.Info("forwarder running", "install-id", installID)
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

func handleEvent(ctx context.Context, ev *corev1.Event, installID string, store *sinkStore, deliv *deliverer, log *slog.Logger) {
	ce, drop, ok := translate(ev, installID)
	if !ok {
		// Only log Kratix-shaped drops; "not-kratix" is the majority of cluster
		// Event volume and would spam logs.
		if drop != "not-kratix" {
			log.Warn("dropped event", "reason", drop, "event", ev.Name, "involved", ev.InvolvedObject.Kind+"/"+ev.InvolvedObject.Name)
		}
		return
	}
	deliv.fanOut(ctx, ce, store.snapshot())
}

func bootstrapSinks(ctx context.Context, cli client.Client, store *sinkStore) error {
	var list eventingv1alpha1.CloudEventSinkList
	if err := cli.List(ctx, &list); err != nil {
		return err
	}
	for i := range list.Items {
		store.upsert(&list.Items[i])
	}
	return nil
}

func pollSinks(ctx context.Context, cli client.Client, store *sinkStore, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var list eventingv1alpha1.CloudEventSinkList
			if err := cli.List(ctx, &list); err != nil {
				log.Warn("sink list failed", "err", err)
				continue
			}
			seen := make(map[string]struct{}, len(list.Items))
			for i := range list.Items {
				store.upsert(&list.Items[i])
				seen[list.Items[i].Name] = struct{}{}
			}
			for _, s := range store.snapshot() {
				if _, ok := seen[s.name]; !ok {
					store.delete(s.name)
				}
			}
		}
	}
}

func loadKubeconfig(path string) (*rest.Config, error) {
	if path == "" {
		return ctrl.GetConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
