// Command bundle-controller materialises a Promise's companion
// resources (agents, configs, supporting Deployments) once the Promise
// is Available.
//
// A producer ships a PromiseBundle CR alongside their Promise. This
// controller watches PromiseBundle resources; for each, it looks up the
// referenced Promise, and once that Promise is Available it server-side-
// applies every companion with the PromiseBundle set as the
// owner-reference. Deleting the bundle cascades to its companions via
// standard Kubernetes garbage collection.
//
// Single replica in v0.1. Companion application rate is low (proportional
// to Promise installs); leader election is a v0.2 concern when multiple
// controller replicas become useful.
package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	kratixv1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	eventingv1alpha1 "github.com/syntasso/kratix/eventing/api/v1alpha1"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(eventingv1alpha1.AddToScheme(scheme))
	utilruntime.Must(kratixv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		kubeconfig   string
		metricsAddr  string
		probeAddr    string
		fieldManager string
	)
	flag.StringVar(&kubeconfig, "kubeconfig-path", "", "path to kubeconfig; empty for in-cluster or KUBECONFIG env. (Renamed to avoid the controller-runtime global --kubeconfig flag collision.)")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address for the metrics endpoint")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address for readiness/liveness probes")
	flag.StringVar(&fieldManager, "field-manager", defaultFieldManager, "SSA field manager name")
	flag.Parse()

	ctrl.SetLogger(zap.New())
	logger := ctrl.Log.WithName("bundle-controller")

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		logger.Error(err, "kubeconfig")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		logger.Error(err, "manager")
		os.Exit(1)
	}

	r := &Reconciler{
		Client:       mgr.GetClient(),
		FieldManager: fieldManager,
	}

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&eventingv1alpha1.PromiseBundle{}).
		Watches(&kratixv1alpha1.Promise{},
			handlerForPromiseEnqueueBundle(mgr),
			builder.WithPredicates()).
		Complete(r); err != nil {
		logger.Error(err, "controller setup")
		os.Exit(1)
	}

	logger.Info("starting bundle-controller manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		logger.Error(err, "manager exited")
		os.Exit(1)
	}
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
