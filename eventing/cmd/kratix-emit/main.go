// Command kratix-emit publishes a Kratix CloudEvent from inside a pipeline
// container. It produces a Kubernetes Event on the parent Promise or Resource
// Request (read from KRATIX_OBJECT_* env vars) with the kratix.io/ce-*
// annotations required by eventing/WIRE-FORMAT.md.
//
// Emission is best-effort: if the API server is unreachable or RBAC denies
// the request, kratix-emit logs to stderr and exits 0. Telemetry that breaks
// pipelines is worse than no telemetry.
//
// Usage:
//
//	kratix-emit \
//	    --type=upstream.fetch.failed \
//	    --severity=warning \
//	    --message="429 from upstream API"
//
// Optional:
//
//	--reason=ExplicitReason            # PascalCase; derived from --type otherwise
//	--correlation-id=<ulid>            # group multiple emits in one chain
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/syntasso/kratix/eventing/pkg/schema"
)

func main() {
	args, corrID, kubeconfig, timeout := parseFlags()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := args.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "kratix-emit: %s\n", err)
		os.Exit(2) // user error, not a transient failure — exit non-zero
	}

	parent, err := readParentFromEnv(os.Getenv)
	if err != nil {
		log.Warn("kratix-emit: parent attribution failed; skipping emission", "err", err)
		os.Exit(0) // best-effort
	}

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		log.Warn("kratix-emit: kubeconfig load failed; skipping emission", "err", err)
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := emit(ctx, cfg, parent, args, corrID); err != nil {
		log.Warn("kratix-emit: emission failed; pipeline continues", "err", err)
		os.Exit(0)
	}
}

func parseFlags() (args emissionArgs, correlationID, kubeconfig string, timeout time.Duration) {
	flag.StringVar(&args.Type, "type", "", "CloudEvent type, dot-segmented (e.g. 'upstream.fetch.failed'). kratix.* is reserved.")
	flag.StringVar(&args.Severity, "severity", schema.SeverityInfo, "info | warning")
	flag.StringVar(&args.Reason, "reason", "", "Event.reason in PascalCase; derived from --type if omitted")
	flag.StringVar(&args.Message, "message", "", "human-readable message")
	flag.StringVar(&correlationID, "correlation-id", "", "groups this emission with related events; auto-generated if empty")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig; empty for in-cluster config")
	flag.DurationVar(&timeout, "timeout", 5*time.Second, "API call timeout; emission gives up after this")
	flag.Parse()
	return args, correlationID, kubeconfig, timeout
}

func emit(ctx context.Context, cfg *rest.Config, parent parentRef, args emissionArgs, corrID string) error {
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("build kube client: %w", err)
	}

	if corrID == "" {
		corrID = newCorrelationID()
	}

	involved := corev1.ObjectReference{
		Kind:       parent.Kind,
		APIVersion: parent.APIVersion,
		Name:       parent.Name,
		Namespace:  parent.Namespace,
	}

	// For cluster-scoped parents (Promises), Events still need a namespace to
	// live in. Use the pod's own namespace from the downward API; the
	// involvedObject.namespace stays empty to reflect cluster scope.
	eventNamespace := parent.Namespace
	if eventNamespace == "" {
		eventNamespace = os.Getenv("POD_NAMESPACE") // may also be empty
		if eventNamespace == "" {
			// Last-ditch: read from the standard service-account mounted file.
			if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
				eventNamespace = string(data)
			}
		}
	}
	if eventNamespace == "" {
		return fmt.Errorf("cannot determine namespace to host the Event")
	}

	now := metav1.NewTime(time.Now())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "kratix-emit-",
			Namespace:    eventNamespace,
			Annotations: map[string]string{
				schema.AnnotationCorrelationID: corrID,
				schema.AnnotationGeneration:    strconv.FormatInt(0, 10), // user pipelines do not own a generation; 0 is the sentinel
				// Pin the CloudEvent type explicitly: user-emitted events live
				// under pipeline.* and cannot be derived from reason naming.
				schema.AnnotationType: args.Type,
			},
		},
		InvolvedObject:      involved,
		Reason:              args.Reason,
		Message:             args.Message,
		Type:                eventTypeForSeverity(args.Severity),
		Source:              corev1.EventSource{Component: "kratix-emit"},
		FirstTimestamp:      now,
		LastTimestamp:       now,
		EventTime:           metav1.NewMicroTime(now.Time),
		ReportingController: "kratix-emit",
		ReportingInstance:   os.Getenv("HOSTNAME"),
		Action:              args.Reason,
	}

	if _, err := kc.CoreV1().Events(eventNamespace).Create(ctx, ev, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create Event: %w", err)
	}
	return nil
}

func loadKubeconfig(path string) (*rest.Config, error) {
	if path == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", path)
}
