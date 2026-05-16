// Command kratix-approve approves an AgentProposal by name.
//
// It wraps `kubectl annotate` with two safety checks the raw command lacks:
//
//   - Refuses if the proposal is already approved (status.approvedBy set).
//   - Refuses if the proposal has expired (status.resolution=expired).
//
// Both checks are advisory — the gate controller is the source of truth
// and applies the same rules — but rejecting at the CLI layer means a
// confused operator gets a clear error instead of a silently-ignored
// annotation.
//
// Usage:
//
//	kratix-approve <proposalId> \
//	    --approver=you@example.com \
//	    --namespace=kratix-platform-system
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
		approver   string
		namespace  string
		kubeconfig string
		timeout    time.Duration
	)
	flag.StringVar(&approver, "approver", "", "approver identity (email, username, etc.)")
	flag.StringVar(&namespace, "namespace", "kratix-platform-system", "namespace of the AgentProposal")
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig; empty for in-cluster or default")
	flag.DurationVar(&timeout, "timeout", 10*time.Second, "API call timeout")
	flag.Parse()

	if flag.NArg() != 1 {
		die("usage: kratix-approve <proposalId> --approver=<who>\n")
	}
	proposalID := flag.Arg(0)
	if approver == "" {
		die("--approver is required\n")
	}

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		die("kubeconfig: %v\n", err)
	}
	cli, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		die("client: %v\n", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := approve(ctx, cli, namespace, proposalID, approver); err != nil {
		die("%v\n", err)
	}
	fmt.Printf("approved %s/%s by %s\n", namespace, proposalID, approver)
}

func approve(ctx context.Context, cli client.Client, namespace, proposalID, approver string) error {
	prop := &eventingv1alpha1.AgentProposal{}
	if err := cli.Get(ctx, client.ObjectKey{Namespace: namespace, Name: proposalID}, prop); err != nil {
		return fmt.Errorf("get proposal: %w", err)
	}

	if prop.Status.Resolution != "" {
		return fmt.Errorf("proposal already %s; not modifying", prop.Status.Resolution)
	}
	if prop.Status.ApprovedBy != "" {
		return fmt.Errorf("proposal already approved by %s", prop.Status.ApprovedBy)
	}
	if existing := prop.GetAnnotations()[eventingv1alpha1.ApprovalAnnotation]; existing != "" {
		return fmt.Errorf("proposal already carries %s=%s", eventingv1alpha1.ApprovalAnnotation, existing)
	}
	if !prop.Spec.ExpiresAt.IsZero() && time.Now().After(prop.Spec.ExpiresAt.Time) {
		return errors.New("proposal expiresAt has passed; approval would be ignored by the gate")
	}

	annotations := prop.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[eventingv1alpha1.ApprovalAnnotation] = approver
	prop.SetAnnotations(annotations)
	if err := cli.Update(ctx, prop); err != nil {
		return fmt.Errorf("annotate: %w", err)
	}
	return nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format, args...)
	os.Exit(1)
}

// loadKubeconfig resolves a *rest.Config without importing
// controller-runtime's package-level init() (which would register a global
// --kubeconfig flag that collides with this binary's own flag set).
func loadKubeconfig(path string) (*rest.Config, error) {
	if path != "" {
		return clientcmd.BuildConfigFromFlags("", path)
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return clientcmd.BuildConfigFromFlags("", env)
	}
	return rest.InClusterConfig()
}

// Force a use of metav1 so a future refactor doesn't accidentally drop
// the import — kubebuilder-generated types regularly need it.
var _ = metav1.Time{}
