// Command job-watcher is the producer of kratix.scheduledjob.* CloudEvents.
//
// It watches:
//   - Jobs owned by CronJobs created by the scheduled-job Promise pipeline
//   - CronJobs created by the scheduled-job Promise pipeline (for the
//     suspend/skipped transition)
//
// For each meaningful state transition, it writes an annotated K8s Event;
// the kratix-event-forwarder picks those up and fans them out to
// CloudEventSinks. The watcher itself has no HTTP surface — the substrate
// is the bridge.
//
// Single replica. No leader election; emitting the same transition twice
// (from a duplicate Pod) is harmless but noisy. Run replicas=1.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	defaultLabelSelector = "platform.kratix.io/owned-by=scheduled-job-promise"
)

func main() {
	var (
		kubeconfig    string
		namespace     string
		labelSelector string
		resync        time.Duration
	)
	flag.StringVar(&kubeconfig, "kubeconfig", "", "path to kubeconfig; empty for in-cluster or KUBECONFIG env")
	flag.StringVar(&namespace, "namespace", "", "namespace to watch; empty = all namespaces")
	flag.StringVar(&labelSelector, "label-selector", defaultLabelSelector, "label selector for owned Jobs/CronJobs")
	flag.DurationVar(&resync, "resync", 5*time.Minute, "informer resync period")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := loadKubeconfig(kubeconfig)
	if err != nil {
		log.Error("kubeconfig", "err", err)
		os.Exit(1)
	}
	kc, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("kube client", "err", err)
		os.Exit(1)
	}

	if _, err := labels.Parse(labelSelector); err != nil {
		log.Error("invalid label selector", "err", err, "selector", labelSelector)
		os.Exit(2)
	}

	w := &watcher{
		kc:            kc,
		log:           log,
		labelSelector: labelSelector,
		prevJobs:      map[types.NamespacedName]*batchv1.Job{},
		prevCronJobs:  map[types.NamespacedName]bool{},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	w.Run(ctx, namespace, resync)
}

type watcher struct {
	kc            kubernetes.Interface
	log           *slog.Logger
	labelSelector string

	mu           sync.Mutex
	prevJobs     map[types.NamespacedName]*batchv1.Job
	prevCronJobs map[types.NamespacedName]bool
}

func (w *watcher) Run(ctx context.Context, namespace string, resync time.Duration) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		w.kc, resync,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(opts *metav1.ListOptions) {
			opts.LabelSelector = w.labelSelector
		}),
	)
	jobInformer := factory.Batch().V1().Jobs().Informer()
	cronInformer := factory.Batch().V1().CronJobs().Informer()

	_, _ = jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.onJob(ctx, nil, obj) },
		UpdateFunc: func(old, new interface{}) { w.onJob(ctx, old, new) },
		// Deletes don't carry a transition — we rely on the final
		// Failed/Completed emission before the Job's history limit kicks in.
	})
	_, _ = cronInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { w.onCronJob(ctx, nil, obj) },
		UpdateFunc: func(old, new interface{}) { w.onCronJob(ctx, old, new) },
	})

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())
	w.log.Info("job-watcher running", "namespace", namespace, "selector", w.labelSelector)

	em := newEmitter(w.kc, namespace)
	_ = em // emitter is held inside on-callbacks via closure below; expose via a field if we add reconcilers.
	<-ctx.Done()
	w.log.Info("shutting down")
}

func (w *watcher) onJob(ctx context.Context, oldObj, newObj interface{}) {
	curr, ok := newObj.(*batchv1.Job)
	if !ok || curr == nil {
		return
	}
	prev, _ := oldObj.(*batchv1.Job)
	if prev == nil {
		// Pull cached prev so re-list events don't re-emit.
		key := types.NamespacedName{Namespace: curr.Namespace, Name: curr.Name}
		w.mu.Lock()
		prev = w.prevJobs[key]
		w.mu.Unlock()
	}

	tr := classifyJob(prev, curr)

	// Update cache regardless of whether we emit — the cache reflects the
	// most recent observation, so the next transition is computed correctly.
	w.mu.Lock()
	w.prevJobs[types.NamespacedName{Namespace: curr.Namespace, Name: curr.Name}] = curr.DeepCopy()
	w.mu.Unlock()

	if tr == nil {
		return
	}
	em := newEmitter(w.kc, curr.Namespace)
	involved := corev1.ObjectReference{
		Kind:       "Job",
		APIVersion: batchv1.SchemeGroupVersion.String(),
		Name:       curr.Name,
		Namespace:  curr.Namespace,
		UID:        curr.UID,
	}
	if err := em.Emit(ctx, tr, involved); err != nil {
		w.log.Warn("emit failed", "type", tr.Type, "job", curr.Name, "err", err)
	}
}

func (w *watcher) onCronJob(ctx context.Context, oldObj, newObj interface{}) {
	curr, ok := newObj.(*batchv1.CronJob)
	if !ok || curr == nil {
		return
	}
	scheduled := curr.Labels["app.kubernetes.io/instance"]
	if scheduled == "" {
		return
	}
	currSuspended := curr.Spec.Suspend != nil && *curr.Spec.Suspend

	key := types.NamespacedName{Namespace: curr.Namespace, Name: curr.Name}
	w.mu.Lock()
	prev := w.prevCronJobs[key]
	w.prevCronJobs[key] = currSuspended
	w.mu.Unlock()

	tr := classifyCronJobSuspended(prev, currSuspended, scheduled, curr.Name)
	if tr == nil {
		return
	}
	em := newEmitter(w.kc, curr.Namespace)
	involved := corev1.ObjectReference{
		Kind:       "CronJob",
		APIVersion: batchv1.SchemeGroupVersion.String(),
		Name:       curr.Name,
		Namespace:  curr.Namespace,
		UID:        curr.UID,
	}
	if err := em.Emit(ctx, tr, involved); err != nil {
		w.log.Warn("emit failed", "type", tr.Type, "cronjob", curr.Name, "err", err)
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

func osHostname() (string, error) { return os.Hostname() }
