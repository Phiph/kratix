/*
Copyright 2021 Syntasso.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/internal/logging"
	"github.com/syntasso/kratix/internal/telemetry"
	"github.com/syntasso/kratix/lib/compression"
	"github.com/syntasso/kratix/lib/writers/dispatch"
	"gopkg.in/yaml.v2"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"go.opentelemetry.io/otel/attribute"
)

const (
	resourcesDir                             = "resources"
	dependenciesDir                          = "dependencies"
	repoCleanupWorkPlacementFinalizer        = "finalizers.workplacement.kratix.io/repo-cleanup"
	kratixFileCleanupWorkPlacementFinalizer  = "finalizers.workplacement.kratix.io/kratix-dot-files-cleanup"
	scheduleSucceededConditionMismatchReason = "DestinationSelectorMismatch"
	scheduleSucceededConditionMismatchMsg    = "Target destination no longer matches destinationSelectors"
)

type StateFile struct {
	Files []string `json:"files"`
}

// WorkPlacementReconciler reconciles a WorkPlacement object.
// VersionCache is shared across reconcile goroutines (MaxConcurrentReconciles > 1),
// so it must be a sync.Map.
type WorkPlacementReconciler struct {
	Client        client.Client
	Log           logr.Logger
	VersionCache  *sync.Map
	EventRecorder record.EventRecorder

	Dispatcher dispatch.Dispatcher
}

type workPlacementReconcileContext struct {
	ctx        context.Context
	controller string

	logger        logr.Logger
	trace         *reconcileTrace
	client        client.Client
	eventRecorder record.EventRecorder

	workPlacement *v1alpha1.WorkPlacement
	destination   *v1alpha1.Destination
	dispatcher    dispatch.Dispatcher

	versionCache *sync.Map
}

//+kubebuilder:rbac:groups=platform.kratix.io,resources=workplacements,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=platform.kratix.io,resources=workplacements/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=platform.kratix.io,resources=workplacements/finalizers,verbs=update

func (r *WorkPlacementReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	logger := r.Log.WithValues(
		"controller", "workPlacement",
		"name", req.Name,
		"namespace", req.Namespace,
	)

	return withTrace(logger, func() (ctrl.Result, error) {
		workPlacementCtx, err := r.newReconcileContext(ctx, logger, req)
		if err != nil {
			logging.Error(logger, err, "error getting WorkPlacement")
			return defaultRequeue, nil
		}

		if workPlacementCtx == nil {
			return ctrl.Result{}, nil
		}
		return workPlacementCtx.reconcileWithSpanAttributes()
	})
}

func (r *WorkPlacementReconciler) newReconcileContext(ctx context.Context, logger logr.Logger, req ctrl.Request) (*workPlacementReconcileContext, error) {
	workPlacement := &v1alpha1.WorkPlacement{}
	if err := r.Client.Get(ctx, req.NamespacedName, workPlacement); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}

	dest := &v1alpha1.Destination{}
	targetDestination := workPlacement.Spec.TargetDestinationName
	logger = logger.WithValues("destination", targetDestination)
	key := client.ObjectKey{Name: targetDestination}

	if err := r.Client.Get(ctx, key, dest); err != nil {
		if apierrors.IsNotFound(err) {
			logging.Warn(logger, "destination not found, cleaning up deletion finalizers")
			cleanupDeletionFinalizers(workPlacement)
			return nil, r.Client.Update(ctx, workPlacement)
		}
		logging.Error(logger, err, "failed to retrieve Destination")
		return nil, err
	}

	return &workPlacementReconcileContext{
		ctx:           ctx,
		controller:    "workplacement-controller",
		logger:        logger.WithValues("generation", workPlacement.GetGeneration()),
		client:        r.Client,
		eventRecorder: r.EventRecorder,
		workPlacement: workPlacement,
		destination:   dest,
		dispatcher:    r.Dispatcher,
		versionCache:  r.VersionCache,
	}, nil
}

func (w *workPlacementReconcileContext) reconcileWithSpanAttributes() (result ctrl.Result, retErr error) {
	promiseName := w.workPlacement.Spec.PromiseName
	resourceName := w.workPlacement.Spec.ResourceName
	spanName := fmt.Sprintf("%s/WorkPlacementReconcile", promiseName)
	if resourceName != "" {
		spanName = fmt.Sprintf("%s/%s", resourceName, spanName)
	}
	w.ctx, w.logger, w.trace = setupReconcileTrace(w.ctx, "workplacement-controller", spanName, w.workPlacement, w.logger)
	defer finishReconcileTrace(w.trace, &retErr)()

	addWorkPlacementSpanAttributes(w.trace, promiseName, w.workPlacement)

	if err := persistReconcileTrace(w.trace, w.client, w.logger); err != nil {
		logging.Error(w.logger, err, "failed to persist trace annotations")
		return ctrl.Result{}, err
	}

	return w.Reconcile()
}

func (w *workPlacementReconcileContext) Reconcile() (result ctrl.Result, retErr error) {
	destKey, err := w.destKey()
	if err != nil {
		logging.Error(w.logger, err, "failed to resolve destination key")
		return defaultRequeue, nil
	}

	if !w.workPlacement.DeletionTimestamp.IsZero() {
		return w.handleDeletion(destKey)
	}

	if missingFinalizers := w.checkWorkPlacementFinalizers(); len(missingFinalizers) > 0 {
		if err := addFinalizers(opts{client: w.client, logger: w.logger, ctx: w.ctx}, w.workPlacement, missingFinalizers); err != nil {
			if !apierrors.IsConflict(err) {
				return ctrl.Result{}, err
			}
		}
		return fastRequeue, nil
	}

	versionID, requeue, err := w.writeToStateStore(destKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue.RequeueAfter > 0 {
		return requeue, nil
	}

	if err := w.updateResourceStatus(versionID, nil); err != nil {
		if apierrors.IsConflict(err) {
			return fastRequeue, nil
		}
		return defaultRequeue, nil
	}

	return ctrl.Result{}, nil
}

// destKey resolves the destination-level DestinationKey for the work placement's
// target destination. The Branch field is populated by looking up the referenced
// GitStateStore (empty for BucketStateStore). Must match the key used by the
// destination controller when it registers with the dispatcher.
func (w *workPlacementReconcileContext) destKey() (dispatch.DestinationKey, error) {
	ref := w.destination.Spec.StateStoreRef
	if ref == nil {
		return dispatch.DestinationKey{}, fmt.Errorf("destination %q has no StateStoreRef", w.destination.Name)
	}
	switch ref.Kind {
	case "GitStateStore":
		ss := &v1alpha1.GitStateStore{}
		if err := w.client.Get(w.ctx, client.ObjectKey{Name: ref.Name}, ss); err != nil {
			return dispatch.DestinationKey{}, fmt.Errorf("failed to get GitStateStore %q: %w", ref.Name, err)
		}
		return dispatch.DestinationKey{
			StateStoreKind: "GitStateStore",
			StateStoreName: ref.Name,
			Branch:         ss.Spec.Branch,
			Path:           w.destination.Spec.Path,
		}, nil
	case "BucketStateStore":
		return dispatch.DestinationKey{
			StateStoreKind: "BucketStateStore",
			StateStoreName: ref.Name,
			Path:           w.destination.Spec.Path,
		}, nil
	default:
		return dispatch.DestinationKey{}, fmt.Errorf("unsupported state store kind %q", ref.Kind)
	}
}

func (w *workPlacementReconcileContext) updateResourceStatus(versionID string, err error) error {
	var updated bool
	var clearVersionCache bool

	if err != nil {
		updated = w.workPlacement.SetWriteFailedCondition(err)
	} else {
		versionID = w.getCachedVersionID(versionID)

		versionChanged := versionID != "" && w.workPlacement.Status.VersionID != versionID
		if versionChanged {
			w.workPlacement.Status.VersionID = versionID
		}
		writeSucceededCondChanged := w.workPlacement.SetWriteSucceededCondition()
		condChanged := w.workPlacement.SetWorkplacementReadyStatus()

		updated = versionChanged || writeSucceededCondChanged || condChanged
		clearVersionCache = true
	}

	if updated {
		logging.Debug(w.logger, "updating workplacement status")
		if err := w.client.Status().Update(w.ctx, w.workPlacement); err != nil {
			return err
		}
	}

	if clearVersionCache {
		w.removeVersionIDFromCache()
	}

	return nil
}

func (w *workPlacementReconcileContext) publishWriteEvent(reason, versionID string, err error) {
	if err != nil {
		w.eventRecorder.Eventf(w.workPlacement, v1.EventTypeWarning, reason,
			fmt.Sprintf("failed writing to Destination: %s with error: %s; check kubectl get destination for more info", w.workPlacement.Spec.TargetDestinationName, err.Error()))
		return
	}

	if versionID != "" {
		w.eventRecorder.Eventf(w.workPlacement, v1.EventTypeNormal, reason,
			"successfully written to Destination: %s with versionID: %s", w.workPlacement.Spec.TargetDestinationName, versionID)
		return
	}

	w.eventRecorder.Eventf(w.workPlacement, v1.EventTypeNormal, reason,
		"successfully written to Destination: %s", w.workPlacement.Spec.TargetDestinationName)
}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkPlacementReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkPlacement{}, builder.WithPredicates(workPlacementReconcilePredicate())).
		WithOptions(controller.Options{MaxConcurrentReconciles: 16}).
		Complete(r)
}

func workPlacementReconcilePredicate() predicate.Funcs {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldDeletionTimestamp := e.ObjectOld.GetDeletionTimestamp()
			newDeletionTimestamp := e.ObjectNew.GetDeletionTimestamp()
			deletionStarted := (oldDeletionTimestamp == nil) != (newDeletionTimestamp == nil)

			return e.ObjectNew.GetGeneration() != e.ObjectOld.GetGeneration() || deletionStarted
		},
	}
}

func (w *workPlacementReconcileContext) checkWorkPlacementFinalizers() []string {
	filepathMode := w.destination.GetFilepathMode()
	var missingFinalizers []string
	if !controllerutil.ContainsFinalizer(w.workPlacement, repoCleanupWorkPlacementFinalizer) {
		missingFinalizers = append(missingFinalizers, repoCleanupWorkPlacementFinalizer)
	}
	if filepathMode == v1alpha1.FilepathModeNone && !controllerutil.ContainsFinalizer(w.workPlacement, kratixFileCleanupWorkPlacementFinalizer) {
		missingFinalizers = append(missingFinalizers, kratixFileCleanupWorkPlacementFinalizer)
	}
	return missingFinalizers
}

func cleanupDeletionFinalizers(workPlacement *v1alpha1.WorkPlacement) {
	if controllerutil.ContainsFinalizer(workPlacement, repoCleanupWorkPlacementFinalizer) {
		controllerutil.RemoveFinalizer(workPlacement, repoCleanupWorkPlacementFinalizer)
	}
	if controllerutil.ContainsFinalizer(workPlacement, kratixFileCleanupWorkPlacementFinalizer) {
		controllerutil.RemoveFinalizer(workPlacement, kratixFileCleanupWorkPlacementFinalizer)
	}
}

func (w *workPlacementReconcileContext) setVersionID(versionID string) {
	if versionID == "" {
		return
	}
	w.versionCache.Store(w.workPlacement.GetUniqueID(), versionID)
}

func (w *workPlacementReconcileContext) getCachedVersionID(versionID string) string {
	if versionID != "" {
		return versionID
	}
	if v, ok := w.versionCache.Load(w.workPlacement.GetUniqueID()); ok {
		return v.(string)
	}
	return ""
}

func (w *workPlacementReconcileContext) removeVersionIDFromCache() {
	w.versionCache.Delete(w.workPlacement.GetUniqueID())
}

func addWorkPlacementSpanAttributes(traceCtx *reconcileTrace, promiseName string, workPlacement *v1alpha1.WorkPlacement) {
	traceCtx.AddAttributes(
		attribute.String("kratix.promise.name", promiseName),
		attribute.String("kratix.workplacement.name", workPlacement.GetName()),
		attribute.String("kratix.workplacement.namespace", workPlacement.GetNamespace()),
		attribute.String("kratix.workplacement.target_destination", workPlacement.Spec.TargetDestinationName),
		attribute.String("kratix.action", traceCtx.Action()),
	)
}

func (w *workPlacementReconcileContext) handleDeletion(destKey dispatch.DestinationKey) (ctrl.Result, error) {
	logging.Info(w.logger, "deleting workplacement")

	if w.destination == nil {
		logging.Debug(w.logger, "destination not found; cleaning up deletion finalizers")
		cleanupDeletionFinalizers(w.workPlacement)
		if err := w.client.Update(w.ctx, w.workPlacement); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return w.deleteWorkPlacement(destKey)
}

func (w *workPlacementReconcileContext) logAndRecordError(err error, reason, message string) {
	logging.Error(w.logger, err, message)
	w.eventRecorder.Eventf(
		w.workPlacement,
		v1.EventTypeWarning,
		reason,
		"%s: %s", message, err.Error(),
	)
}

// buildStateFileWorkload returns the .kratix state-file workload that records
// the current set of paths written by this WorkPlacement.
func (w *workPlacementReconcileContext) buildStateFileWorkload(pathPrefix string) (v1alpha1.Workload, error) {
	workloadPaths := make([]string, 0, len(w.workPlacement.Spec.Workloads))
	for _, workload := range w.workPlacement.Spec.Workloads {
		workloadPaths = append(workloadPaths, filepath.Join(pathPrefix, workload.Filepath))
	}
	newStateFile := StateFile{Files: workloadPaths}
	stateFileContent, marshalErr := yaml.Marshal(newStateFile)
	if marshalErr != nil {
		return v1alpha1.Workload{}, fmt.Errorf("failed to marshal new .kratix state file: %w", marshalErr)
	}
	return v1alpha1.Workload{
		Filepath: w.kratixStateFilePath(),
		Content:  string(stateFileContent),
	}, nil
}

func (w *workPlacementReconcileContext) kratixStateFilePath() string {
	return filepath.Join(w.destination.Spec.Path, fmt.Sprintf(".kratix/%s-%s.yaml", w.workPlacement.Namespace, w.workPlacement.Name))
}

// decodeStateFile parses the .kratix state-file bytes returned by the worker
// for the configured kratixStateFilePath. Missing or empty entries are
// treated as a zero-value StateFile.
func (w *workPlacementReconcileContext) decodeStateFile(reads map[string][]byte) (StateFile, error) {
	data, ok := reads[w.kratixStateFilePath()]
	if !ok || len(data) == 0 {
		return StateFile{}, nil
	}
	stateFile := StateFile{}
	if err := yaml.Unmarshal(data, &stateFile); err != nil {
		w.logAndRecordError(err, "FailedUnmarshalKratixStateFile", "failed to unmarshal .kratix state file")
		return StateFile{}, err
	}
	return stateFile, nil
}

func (w *workPlacementReconcileContext) getAggregatedWorkload() (v1alpha1.Workload, error) {
	filename := w.destination.Spec.Filepath.Filename
	if filename == "" {
		filename = "aggregated.yaml"
	}
	workload := v1alpha1.Workload{
		Filepath: filepath.Join(w.destination.Spec.Path, filename),
	}
	activeWorkplacements, err := w.getAllWorkplacementsForDestination()
	if err != nil {
		w.logAndRecordError(err, "FailedGetAllWorkplacementsForDestination", "failed to get all workplacements for destination")
		return v1alpha1.Workload{}, err
	}

	if len(activeWorkplacements) == 0 {
		return workload, nil
	}

	combinedWorkloads, err := w.combineAllWorkloads(activeWorkplacements)
	if err != nil {
		w.logAndRecordError(err, "FailedGenerateAggregatedWorkloadYAML", "failed to generate aggregated workload yaml")
		return v1alpha1.Workload{}, err
	}

	workload.Content = combinedWorkloads
	return workload, nil
}

func (w *workPlacementReconcileContext) getAllWorkplacementsForDestination() ([]v1alpha1.WorkPlacement, error) {
	allWorkplacements := &v1alpha1.WorkPlacementList{}
	opts := &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{
			TargetDestinationNameLabel: w.destination.Name,
		}),
	}

	if err := w.client.List(w.ctx, allWorkplacements, opts); err != nil {
		return nil, fmt.Errorf("failed to list all WorkPlacements: %w", err)
	}

	var active []v1alpha1.WorkPlacement
	for _, wp := range allWorkplacements.Items {
		if wp.DeletionTimestamp.IsZero() {
			active = append(active, wp)
		}
	}
	// sort active workplacements by name
	sort.Slice(active, func(i, j int) bool {
		return active[i].Name < active[j].Name
	})
	return active, nil
}

func (w *workPlacementReconcileContext) combineAllWorkloads(workPlacements []v1alpha1.WorkPlacement) (string, error) {
	combinedWorkloads := []v1alpha1.Workload{}
	for _, wp := range workPlacements {
		for _, workload := range wp.Spec.Workloads {
			decompressedContent, err := compression.DecompressContent([]byte(workload.Content))
			if err != nil {
				return "", fmt.Errorf("unable to decompress file content: %w", err)
			}

			workload.Content = string(decompressedContent)
			combinedWorkloads = append(combinedWorkloads, workload)
		}
	}

	var sb strings.Builder

	for i, workload := range combinedWorkloads {
		if i > 0 {
			sb.WriteString("\n---\n")
		}
		sb.WriteString(workload.Content)
	}

	return sb.String(), nil
}

// submit wraps dispatcher.Submit and maps ErrDestinationNotRegistered to a
// transient requeue (the destination's state-store reconcile has not caught
// up yet).
func (w *workPlacementReconcileContext) submit(destKey dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, ctrl.Result, error) {
	res, err := w.dispatcher.Submit(w.ctx, destKey, intent)
	if err != nil {
		if errors.Is(err, dispatch.ErrDestinationNotRegistered) {
			logging.Debug(w.logger, "destination not registered with dispatcher, requeuing")
			return dispatch.Result{}, defaultRequeue, nil
		}
		return dispatch.Result{}, ctrl.Result{}, err
	}
	return res, ctrl.Result{}, nil
}

func (w *workPlacementReconcileContext) writeToStateStore(destKey dispatch.DestinationKey) (string, ctrl.Result, error) {
	metricAttrs := telemetry.WorkPlacementWriteAttributes(
		w.workPlacement.Spec.PromiseName,
		w.workPlacement.Spec.ResourceName,
		w.workPlacement.Spec.TargetDestinationName,
		w.workPlacement.PipelineName(),
	)

	logging.Debug(w.logger, "writing files to state store")
	versionID, workloadErr := w.writeWorkloadsToStateStore(destKey)
	if workloadErr != nil {
		telemetry.RecordWorkPlacementWrite(w.ctx, telemetry.WorkPlacementWriteResultFailure, metricAttrs...)
		w.logAndRecordError(workloadErr, "FailedWriteToDestination", "error writing to destination; check kubectl get destination for more info")
		return "", defaultRequeue, w.updateResourceStatus("", workloadErr)
	}

	telemetry.RecordWorkPlacementWrite(w.ctx, telemetry.WorkPlacementWriteResultSuccess, metricAttrs...)
	w.setVersionID(versionID)
	w.publishWriteEvent("WorkloadsWrittenToStateStore", versionID, nil)

	return versionID, ctrl.Result{}, nil
}

func (w *workPlacementReconcileContext) writeWorkloadsToStateStore(destKey dispatch.DestinationKey) (string, error) {
	intent, err := w.buildWriteIntent()
	if err != nil {
		return "", err
	}
	res, requeue, err := w.submit(destKey, intent)
	if err != nil {
		logging.Error(w.logger, err, "error writing resources to repository")
		return "", err
	}
	if requeue.RequeueAfter > 0 {
		// Destination not yet registered with the dispatcher; signal via
		// empty version, no error. The caller will skip status updates by
		// returning the requeue; we currently always submit so just return.
		return "", nil
	}
	return res.VersionID, nil
}

// buildWriteIntent constructs the dispatch.Intent that captures the write
// behaviour for the configured filepath mode. The Decide callback closes over
// w (the reconcile context); it is invoked synchronously by the worker
// during Submit, which blocks the reconcile goroutine until the batch
// completes, so the closure outlives the call safely.
func (w *workPlacementReconcileContext) buildWriteIntent() (dispatch.Intent, error) {
	switch w.destination.GetFilepathMode() {
	case v1alpha1.FilepathModeAggregatedYAML:
		workload, err := w.getAggregatedWorkload()
		if err != nil {
			return dispatch.Intent{}, err
		}
		return dispatch.Intent{
			WorkPlacement: w.workPlacement.Name,
			SubDir:        "",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				return dispatch.Writes{ToCreate: []v1alpha1.Workload{workload}}, nil
			},
		}, nil

	case v1alpha1.FilepathModeNone:
		pathPrefix := w.destination.Spec.Path + "/"
		kratixPath := w.kratixStateFilePath()
		return dispatch.Intent{
			WorkPlacement: w.workPlacement.Name,
			SubDir:        "",
			Reads:         []string{kratixPath},
			Decide: func(reads map[string][]byte) (dispatch.Writes, error) {
				oldStateFile, err := w.decodeStateFile(reads)
				if err != nil {
					return dispatch.Writes{}, err
				}
				stateFileWorkload, err := w.buildStateFileWorkload(pathPrefix)
				if err != nil {
					return dispatch.Writes{}, err
				}
				workloadsToCreate := []v1alpha1.Workload{stateFileWorkload}
				for _, workload := range w.workPlacement.Spec.Workloads {
					decompressedContent, err := compression.DecompressContent([]byte(workload.Content))
					if err != nil {
						return dispatch.Writes{}, fmt.Errorf("unable to decompress file content: %w", err)
					}

					workload.Content = string(decompressedContent)
					workload.Filepath = filepath.Join(pathPrefix, workload.Filepath)
					workloadsToCreate = append(workloadsToCreate, workload)
				}
				workloadsToDelete := cleanupWorkloadsWithPrefix(oldStateFile.Files, w.workPlacement.Spec.Workloads, pathPrefix)
				return dispatch.Writes{ToCreate: workloadsToCreate, ToDelete: workloadsToDelete}, nil
			},
		}, nil

	case v1alpha1.FilepathModeNestedByMetadata:
		logging.Trace(w.logger, "handling file path mode nestedByMetadata")
		return dispatch.Intent{
			WorkPlacement: w.workPlacement.Name,
			SubDir:        getDir(w.destination.Spec.Path, *w.workPlacement) + "/",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				workloadsToCreate := make([]v1alpha1.Workload, 0, len(w.workPlacement.Spec.Workloads))
				for _, workload := range w.workPlacement.Spec.Workloads {
					decompressedContent, err := compression.DecompressContent([]byte(workload.Content))
					if err != nil {
						return dispatch.Writes{}, fmt.Errorf("unable to decompress file content: %w", err)
					}

					workload.Content = string(decompressedContent)
					workloadsToCreate = append(workloadsToCreate, workload)
				}
				return dispatch.Writes{ToCreate: workloadsToCreate}, nil
			},
		}, nil

	default:
		return dispatch.Intent{}, fmt.Errorf("unsupported file path mode: %s", w.destination.GetFilepathMode())
	}
}

func (w *workPlacementReconcileContext) deleteWorkPlacement(destKey dispatch.DestinationKey) (ctrl.Result, error) {
	pendingRepoCleanup := controllerutil.ContainsFinalizer(w.workPlacement, repoCleanupWorkPlacementFinalizer)
	pendingKratixFileCleanup := controllerutil.ContainsFinalizer(w.workPlacement, kratixFileCleanupWorkPlacementFinalizer)

	workloadsToDelete := []string{}

	if pendingRepoCleanup {
		logging.Debug(w.logger, "deleting files from repository")
		filePathMode := w.destination.GetFilepathMode()

		switch filePathMode {

		case v1alpha1.FilepathModeNestedByMetadata:
			logging.Trace(w.logger, "handling file path mode nestedByMetadata")
			// The writer purges the entire SubDir prefix when ToCreate is empty.
			// Use the same SubDir as the create path so dedup keys line up.
			dir := getDir(w.destination.Spec.Path, *w.workPlacement) + "/"
			purgeIntent := dispatch.Intent{
				WorkPlacement: w.workPlacement.Name,
				SubDir:        dir,
				Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
					return dispatch.Writes{}, nil
				},
			}
			_, requeue, err := w.submit(destKey, purgeIntent)
			if err != nil {
				w.logAndRecordError(err, "FailedDeleteFilesFromRepository", "failed to delete files from repository")
				return defaultRequeue, nil
			}
			if requeue.RequeueAfter > 0 {
				return requeue, nil
			}

		case v1alpha1.FilepathModeNone:
			logging.Trace(w.logger, "handling file path mode none")
			capturedFiles, requeue, err := w.fetchStateFile(destKey)
			if err != nil {
				logging.Debug(w.logger, "failed to read .kratix state file", "error", err)
				return defaultRequeue, nil
			}
			if requeue.RequeueAfter > 0 {
				return requeue, nil
			}
			workloadsToDelete = capturedFiles

		case v1alpha1.FilepathModeAggregatedYAML:
			logging.Trace(w.logger, "handling file path mode aggregatedYAML")
			workload, err := w.getAggregatedWorkload()
			if err != nil {
				logging.Debug(w.logger, "failed to get aggregated workload", "error", err)
				return defaultRequeue, nil
			}
			workloadsToDelete = []string{workload.Filepath}

			if workload.Content != "" { // there are still other workplacements for this destination
				updateIntent := dispatch.Intent{
					WorkPlacement: w.workPlacement.Name,
					SubDir:        "",
					Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
						return dispatch.Writes{ToCreate: []v1alpha1.Workload{workload}}, nil
					},
				}
				_, requeue, err := w.submit(destKey, updateIntent)
				if err != nil {
					logging.Debug(w.logger, "failed to update files in repository", "error", err)
					return defaultRequeue, nil
				}
				if requeue.RequeueAfter > 0 {
					return requeue, nil
				}
				workloadsToDelete = []string{}
			}

		default:
			return ctrl.Result{}, fmt.Errorf("unsupported file path mode: %s", filePathMode)
		}
	}

	if pendingKratixFileCleanup {
		logging.Debug(w.logger, "cleaning up .kratix state file")
		workloadsToDelete = append(workloadsToDelete, w.kratixStateFilePath())
	}

	return w.delete(destKey, workloadsToDelete)
}

// fetchStateFile submits a read-only intent with a no-op Decide and reads the
// .kratix state-file paths from the live destination. It returns the file
// paths the worker discovered there. Used by the deletion path to know which
// files to remove.
func (w *workPlacementReconcileContext) fetchStateFile(destKey dispatch.DestinationKey) ([]string, ctrl.Result, error) {
	kratixPath := w.kratixStateFilePath()
	var captured StateFile
	var decodeErr error
	intent := dispatch.Intent{
		WorkPlacement: w.workPlacement.Name,
		SubDir:        "",
		Reads:         []string{kratixPath},
		Decide: func(reads map[string][]byte) (dispatch.Writes, error) {
			captured, decodeErr = w.decodeStateFile(reads)
			return dispatch.Writes{}, decodeErr
		},
	}
	_, requeue, err := w.submit(destKey, intent)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if requeue.RequeueAfter > 0 {
		return nil, requeue, nil
	}
	if decodeErr != nil {
		return nil, ctrl.Result{}, decodeErr
	}
	return captured.Files, ctrl.Result{}, nil
}

func (w *workPlacementReconcileContext) delete(destKey dispatch.DestinationKey, workloadsToDelete []string) (ctrl.Result, error) {
	if len(workloadsToDelete) > 0 {
		intent := dispatch.Intent{
			WorkPlacement: w.workPlacement.Name,
			SubDir:        "",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				return dispatch.Writes{ToDelete: workloadsToDelete}, nil
			},
		}
		_, requeue, err := w.submit(destKey, intent)
		if err != nil {
			w.logAndRecordError(err, "FailedDeleteFilesFromRepository", "failed to delete files from repository")
			return defaultRequeue, nil
		}
		if requeue.RequeueAfter > 0 {
			return requeue, nil
		}
	}

	controllerutil.RemoveFinalizer(w.workPlacement, repoCleanupWorkPlacementFinalizer)
	controllerutil.RemoveFinalizer(w.workPlacement, kratixFileCleanupWorkPlacementFinalizer)

	if err := w.client.Update(w.ctx, w.workPlacement); err != nil {
		if apierrors.IsConflict(err) {
			return defaultRequeue, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func cleanupWorkloadsWithPrefix(oldWorkloads []string, newWorkloads []v1alpha1.Workload, pathPrefix string) []string {
	works := make(map[string]bool)
	for _, w := range newWorkloads {
		works[filepath.Join(pathPrefix, w.Filepath)] = true
	}
	var result []string
	for _, old := range oldWorkloads {
		oldFullPath := old
		if !strings.HasPrefix(old, pathPrefix) {
			oldFullPath = filepath.Join(pathPrefix, old)
		}
		if _, ok := works[oldFullPath]; !ok {
			result = append(result, oldFullPath)
		}
	}
	return result
}

func getDir(destinationPath string, workPlacement v1alpha1.WorkPlacement) string {
	if workPlacement.Spec.ResourceName == "" {
		// destinationPath/dependencies/<promise-name>/<pipeline-name>/<dir-sha>/
		return filepath.Join(destinationPath, dependenciesDir, workPlacement.Spec.PromiseName, workPlacement.PipelineName(), shortID(workPlacement.Spec.ID))
	} else {
		// destinationPath/resources/<rr-namespace>/<promise-name>/<rr-name>/<pipeline-name>/<dir-sha>/
		return filepath.Join(destinationPath, resourcesDir, workPlacement.GetNamespace(), workPlacement.Spec.PromiseName, workPlacement.Spec.ResourceName, workPlacement.PipelineName(), shortID(workPlacement.Spec.ID))
	}
}
