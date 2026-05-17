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

package controller_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/syntasso/kratix/internal/controller"
	"github.com/syntasso/kratix/internal/telemetry"
	"github.com/syntasso/kratix/lib/writers/dispatch"
	"github.com/syntasso/kratix/lib/writers/dispatch/dispatchfakes"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/compression"
	"github.com/syntasso/kratix/lib/hash"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	//+kubebuilder:scaffold:imports
)

// submitCall captures the inputs a reconciler made to dispatcher.Submit so the
// test body can inspect the Intent (dedup key, reads, decided Writes).
type submitCall struct {
	dest   dispatch.DestinationKey
	intent dispatch.Intent
}

// decidedWrites runs intent.Decide against the supplied reads map and
// returns the produced Writes. The reconciler's closures are safe to invoke
// after Submit returns: they capture the reconcile context's fields only.
func decidedWrites(intent dispatch.Intent, reads map[string][]byte) dispatch.Writes {
	ExpectWithOffset(1, intent.Decide).NotTo(BeNil())
	writes, err := intent.Decide(reads)
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	return writes
}

var _ = Describe("WorkPlacementReconciler", func() {
	var (
		ctx                   context.Context
		workloads             []v1alpha1.Workload
		decompressedWorkloads []v1alpha1.Workload
		destination           v1alpha1.Destination
		gitStateStore         v1alpha1.GitStateStore
		bucketStateStore      v1alpha1.BucketStateStore
		workplacementRecorder *record.FakeRecorder

		workPlacementName = "test-work-placement"
		workPlacement     v1alpha1.WorkPlacement
		reconciler        *controller.WorkPlacementReconciler
		fakeDispatcher    *dispatchfakes.FakeDispatcher

		metricsReader        *sdkmetric.ManualReader
		restoreMeterProvider func()
	)

	// stubStateFileReads installs a SubmitStub that, when an intent declares the
	// .kratix state-file path in Reads, presents the supplied bytes to the
	// Decide callback. Intents without Reads use an empty reads map.
	stubStateFileReads := func(path string, data []byte) {
		fakeDispatcher.SubmitCalls(func(_ context.Context, _ dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, error) {
			reads := map[string][]byte{}
			for _, p := range intent.Reads {
				if p == path {
					reads[p] = data
				}
			}
			if _, err := intent.Decide(reads); err != nil {
				return dispatch.Result{}, err
			}
			return dispatch.Result{}, nil
		})
	}

	BeforeEach(func() {
		telemetry.ResetWorkPlacementMetricsForTest()
		var restore func()
		metricsReader, restore = setupTestMeterProvider()
		restoreMeterProvider = restore
		fakeDispatcher = &dispatchfakes.FakeDispatcher{}
		// Default: invoke Decide with an empty reads map so the reconciler
		// runs end-to-end without test-specific stubbing.
		fakeDispatcher.SubmitCalls(func(_ context.Context, _ dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, error) {
			if intent.Decide != nil {
				if _, err := intent.Decide(map[string][]byte{}); err != nil {
					return dispatch.Result{}, err
				}
			}
			return dispatch.Result{}, nil
		})

		ctx = context.Background()
		workplacementRecorder = record.NewFakeRecorder(1024)
		reconciler = &controller.WorkPlacementReconciler{
			Client:        fakeK8sClient,
			Log:           ctrl.Log.WithName("controllers").WithName("Workplacement"),
			VersionCache:  &sync.Map{},
			EventRecorder: workplacementRecorder,
			Dispatcher:    fakeDispatcher,
		}

		compressedContent, err := compression.CompressContent([]byte("{someApi: foo, someValue: bar}"))
		Expect(err).ToNot(HaveOccurred())

		compressedContent2, err := compression.CompressContent([]byte("{someOtherApi: fooz, someOtherValue: barz}"))
		Expect(err).ToNot(HaveOccurred())

		workloads = []v1alpha1.Workload{
			{
				Filepath: "fruit.yaml",
				Content:  string(compressedContent),
			},
			{
				Filepath: "file2.yaml",
				Content:  string(compressedContent2),
			},
		}

		decompressedWorkloads = []v1alpha1.Workload{
			{
				Filepath: "fruit.yaml",
				Content:  "{someApi: foo, someValue: bar}",
			},
			{
				Filepath: "file2.yaml",
				Content:  "{someOtherApi: fooz, someOtherValue: barz}",
			},
		}

		workPlacement = createWorkPlacement(workPlacementName, workloads)

		Expect(fakeK8sClient.Create(ctx, &workPlacement)).To(Succeed())

		destination = v1alpha1.Destination{
			TypeMeta: metav1.TypeMeta{
				Kind:       "Destination",
				APIVersion: "platform.kratix.io/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name: "test-destination",
			},
			Spec: v1alpha1.DestinationSpec{
				Filepath: v1alpha1.Filepath{
					Mode: v1alpha1.FilepathModeNone,
				},
				StateStoreRef: &v1alpha1.StateStoreReference{},
				Path:          "test-path",
			},
		}
	})

	AfterEach(func() {
		if restoreMeterProvider != nil {
			restoreMeterProvider()
		}
	})

	// collectSubmits walks every Submit invocation recorded by the fake and
	// returns them in call order.
	collectSubmits := func() []submitCall {
		calls := make([]submitCall, 0, fakeDispatcher.SubmitCallCount())
		for i := 0; i < fakeDispatcher.SubmitCallCount(); i++ {
			_, dest, intent := fakeDispatcher.SubmitArgsForCall(i)
			calls = append(calls, submitCall{dest: dest, intent: intent})
		}
		return calls
	}

	When("the destination statestore is s3", func() {
		When("the destination has filepath mode of none", func() {
			BeforeEach(func() {
				Expect(fakeK8sClient.Create(ctx, &corev1.Secret{
					TypeMeta: metav1.TypeMeta{
						Kind:       "Secret",
						APIVersion: "metav1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-secret",
						Namespace: "default",
					},
					Data: map[string][]byte{
						"accessKeyID":     []byte("test-access"),
						"secretAccessKey": []byte("test-secret"),
					},
				})).To(Succeed())

				bucketStateStore = v1alpha1.BucketStateStore{
					TypeMeta: metav1.TypeMeta{
						Kind:       "BucketStateStore",
						APIVersion: "platform.kratix.io/v1alpha1",
					},
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-state-store",
					},
					Spec: v1alpha1.BucketStateStoreSpec{
						BucketName: "test-bucket",
						StateStoreCoreFields: v1alpha1.StateStoreCoreFields{
							SecretRef: &corev1.SecretReference{
								Name:      "test-secret",
								Namespace: "default",
							},
						},
						Endpoint: "localhost:9000",
					},
				}
				Expect(fakeK8sClient.Create(ctx, &bucketStateStore)).To(Succeed())

				destination.Spec.StateStoreRef.Kind = "BucketStateStore"
				destination.Spec.StateStoreRef.Name = "test-state-store"
				Expect(fakeK8sClient.Create(ctx, &destination)).To(Succeed())
			})

			It("reconciles", func() {
				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				By("submitting an Intent to the dispatcher")
				submits := collectSubmits()
				// Two reconcile passes: first adds finalizers, second writes.
				Expect(submits).ToNot(BeEmpty())
				writeCall := submits[len(submits)-1]
				Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
				Expect(writeCall.intent.SubDir).To(Equal(""))
				Expect(writeCall.intent.Reads).To(ConsistOf(fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)))

				By("writing workloads files and kratix state file in destination path with no extra dir")
				pathPrefix := destination.Spec.Path + "/"
				expectedWorkloads := []v1alpha1.Workload{
					{Filepath: pathPrefix + "fruit.yaml", Content: "{someApi: foo, someValue: bar}"},
					{Filepath: pathPrefix + "file2.yaml", Content: "{someOtherApi: fooz, someOtherValue: barz}"},
					{
						Filepath: fmt.Sprintf("%s.kratix/%s-%s.yaml", pathPrefix, workPlacement.Namespace, workPlacement.Name),
						Content: `files:
- test-path/fruit.yaml
- test-path/file2.yaml
`,
					},
				}
				writes := decidedWrites(writeCall.intent, map[string][]byte{})
				Expect(writes.ToCreate).To(ConsistOf(expectedWorkloads))
				Expect(writes.ToDelete).To(BeNil())

				By("targeting the right destination key")
				Expect(writeCall.dest.StateStoreKind).To(Equal("BucketStateStore"))
				Expect(writeCall.dest.StateStoreName).To(Equal("test-state-store"))
				Expect(writeCall.dest.Path).To(Equal(destination.Spec.Path))

				By("setting the finalizer")
				wp := &v1alpha1.WorkPlacement{}
				Expect(fakeK8sClient.Get(ctx, types.NamespacedName{Name: workPlacementName, Namespace: "default"}, wp)).
					To(Succeed())
				Expect(wp.GetFinalizers()).To(ConsistOf(
					"finalizers.workplacement.kratix.io/repo-cleanup",
					"finalizers.workplacement.kratix.io/kratix-dot-files-cleanup",
				))

				counts := collectWorkPlacementWriteMetrics(ctx, metricsReader)
				// reconcileUntilCompletion drives N reconcile passes; the
				// success count tracks one per pass that actually wrote.
				Expect(counts).To(HaveKeyWithValue(telemetry.WorkPlacementWriteResultSuccess, BeNumerically(">=", int64(1))))
				Expect(counts).NotTo(HaveKey(telemetry.WorkPlacementWriteResultFailure))
			})

			It("records a failure metric when the state store write fails", func() {
				fakeDispatcher.SubmitReturns(dispatch.Result{}, fmt.Errorf("boom"))

				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement, &opts{requeueExpected: true})
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(Equal(15 * time.Second))

				counts := collectWorkPlacementWriteMetrics(ctx, metricsReader)
				// Every reconcile pass past the finalizer step records a
				// failure. reconcileUntilCompletion drives 30 attempts.
				Expect(counts).To(HaveKeyWithValue(telemetry.WorkPlacementWriteResultFailure, int64(30)))
				Expect(counts).NotTo(HaveKey(telemetry.WorkPlacementWriteResultSuccess))
			})

			When("deleting a work placement", func() {
				BeforeEach(func() {
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("submits a delete intent containing the files from the .kratix state file", func() {
					kratixPath := fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)
					stubStateFileReads(kratixPath, []byte(fmt.Sprintf(`
files:
  - %s/fruit.yaml
  - %s/file2.yaml`, destination.Spec.Path, destination.Spec.Path)))

					submitsBeforeDelete := fakeDispatcher.SubmitCallCount()

					Expect(fakeK8sClient.Delete(ctx, &workPlacement)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					// Two new submits expected: the read of the state file (no-op
					// Decide) and the actual delete intent.
					Expect(len(submits)).To(BeNumerically(">=", submitsBeforeDelete+2))
					deleteCall := submits[len(submits)-1]
					Expect(deleteCall.intent.WorkPlacement).To(Equal(workPlacement.Name))

					writes := decidedWrites(deleteCall.intent, map[string][]byte{})
					Expect(writes.ToDelete).To(ConsistOf(
						fmt.Sprintf("%s/fruit.yaml", destination.Spec.Path),
						fmt.Sprintf("%s/file2.yaml", destination.Spec.Path),
						kratixPath,
					))
				})

				When("the Destination does not exists", func() {
					It("removes the repo-cleanup and kratix-dot-files-cleanup finalizers", func() {
						Expect(fakeK8sClient.Delete(ctx, &destination)).To(Succeed())
						Expect(fakeK8sClient.Delete(ctx, &workPlacement)).To(Succeed())

						_, err := reconciler.Reconcile(ctx,
							ctrl.Request{NamespacedName: types.NamespacedName{Name: workPlacement.GetName(),
								Namespace: workPlacement.GetNamespace()}},
						)
						Expect(err).ToNot(HaveOccurred())

						err = fakeK8sClient.Get(
							ctx,
							types.NamespacedName{
								Name:      workPlacement.GetName(),
								Namespace: "default",
							},
							&workPlacement)
						Expect(err).To(HaveOccurred())
						Expect(errors.IsNotFound(err)).To(BeTrue())
					})
				})
			})

			When("statestore and workplacement.spec.workloads has diverged", func() {
				It("reflects workplacement.spec.workloads", func() {
					kratixPath := fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)
					stubStateFileReads(kratixPath, []byte(`
files:
  - banana.yaml
  - apple.yaml
  - fruit.yaml`))

					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					writeCall := submits[len(submits)-1]
					Expect(writeCall.intent.Reads).To(ConsistOf(kratixPath))
					Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
					Expect(writeCall.intent.SubDir).To(Equal(""))

					pathPrefix := destination.Spec.Path + "/"
					expectedWorkloads := []v1alpha1.Workload{
						{Filepath: pathPrefix + "fruit.yaml", Content: "{someApi: foo, someValue: bar}"},
						{Filepath: pathPrefix + "file2.yaml", Content: "{someOtherApi: fooz, someOtherValue: barz}"},
						{
							Filepath: fmt.Sprintf("%s.kratix/%s-%s.yaml", pathPrefix, workPlacement.Namespace, workPlacement.Name),
							Content: `files:
- test-path/fruit.yaml
- test-path/file2.yaml
`,
						},
					}
					// Replay Decide with the stubbed state-file bytes so the
					// computed deletes match the divergent old state.
					writes := decidedWrites(writeCall.intent, map[string][]byte{
						kratixPath: []byte(`
files:
  - banana.yaml
  - apple.yaml
  - fruit.yaml`),
					})
					Expect(writes.ToCreate).To(ConsistOf(expectedWorkloads))
					Expect(writes.ToDelete).To(ConsistOf(
						fmt.Sprintf("%s/banana.yaml", destination.Spec.Path),
						fmt.Sprintf("%s/apple.yaml", destination.Spec.Path),
					))
				})
			})

			When("multiple workplacements share the destination", func() {
				It("each workplacement only deletes its own files on update", func() {
					_, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())

					otherContent, err := compression.CompressContent([]byte("{other: content}"))
					Expect(err).ToNot(HaveOccurred())
					workPlacementB := createWorkPlacement("test-work-placement-b", []v1alpha1.Workload{{
						Filepath: "other.yaml",
						Content:  string(otherContent),
					}})
					Expect(fakeK8sClient.Create(ctx, &workPlacementB)).To(Succeed())

					_, err = t.reconcileUntilCompletion(reconciler, &workPlacementB)
					Expect(err).NotTo(HaveOccurred())

					Expect(fakeK8sClient.Get(ctx, client.ObjectKeyFromObject(&workPlacement), &workPlacement)).To(Succeed())
					workPlacement.Spec.Workloads = []v1alpha1.Workload{workloads[0]}
					Expect(fakeK8sClient.Update(ctx, &workPlacement)).To(Succeed())

					kratixPath := fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)
					stubStateFileReads(kratixPath, []byte(fmt.Sprintf(`
files:
  - %s/fruit.yaml
  - %s/file2.yaml`, destination.Spec.Path, destination.Spec.Path)))

					_, err = t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())

					submits := collectSubmits()
					last := submits[len(submits)-1]
					Expect(last.intent.WorkPlacement).To(Equal(workPlacement.Name))
					writes := decidedWrites(last.intent, map[string][]byte{
						kratixPath: []byte(fmt.Sprintf(`
files:
  - %s/fruit.yaml
  - %s/file2.yaml`, destination.Spec.Path, destination.Spec.Path)),
					})
					Expect(writes.ToDelete).To(ConsistOf(fmt.Sprintf("%s/file2.yaml", destination.Spec.Path)))
					Expect(writes.ToDelete).NotTo(ContainElement(fmt.Sprintf("%s/other.yaml", destination.Spec.Path)))
				})
			})
		})
	})

	When("the destination statestore is git", func() {
		When("the destination has filepath mode of nestedByMetadata", func() {
			BeforeEach(func() {
				destination.Spec.Filepath.Mode = v1alpha1.FilepathModeNestedByMetadata
				setupGitDestination(&gitStateStore, &destination)
			})

			It("creates files with directory nesting to make each workplacement unique", func() {
				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				submits := collectSubmits()
				Expect(submits).NotTo(BeEmpty())
				writeCall := submits[len(submits)-1]
				Expect(writeCall.intent.SubDir).To(ContainSubstring("test-path/resources/default/test-promise/test-resource"))
				Expect(writeCall.intent.SubDir).To(HaveSuffix("/"))
				Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
				Expect(writeCall.intent.Reads).To(BeNil())

				writes := decidedWrites(writeCall.intent, map[string][]byte{})
				Expect(writes.ToCreate).To(Equal(decompressedWorkloads))
				Expect(writes.ToDelete).To(BeEmpty())
			})

			It("targets the right state store", func() {
				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				submits := collectSubmits()
				Expect(submits).NotTo(BeEmpty())
				dest := submits[len(submits)-1].dest
				Expect(dest.StateStoreKind).To(Equal("GitStateStore"))
				Expect(dest.StateStoreName).To(Equal("test-state-store"))
				Expect(dest.Branch).To(Equal("main"))
				Expect(dest.Path).To(Equal(destination.Spec.Path))
			})

			When("the work placement is for a promise", func() {
				It("uses the promise directory structure", func() {
					workPlacement.Spec.ResourceName = ""
					Expect(fakeK8sClient.Update(ctx, &workPlacement)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					Expect(submits).NotTo(BeEmpty())
					writeCall := submits[len(submits)-1]
					Expect(writeCall.intent.SubDir).To(Equal("test-path/dependencies/test-promise/5058f/"))
					Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
					writes := decidedWrites(writeCall.intent, map[string][]byte{})
					Expect(writes.ToCreate).To(Equal(decompressedWorkloads))
					Expect(writes.ToDelete).To(BeEmpty())
				})
			})
		})

		When("the destination has filepath mode of none", func() {
			BeforeEach(func() {
				setupGitDestination(&gitStateStore, &destination)
			})

			It("reconciles with subDir empty and full paths for per-file delete", func() {
				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				submits := collectSubmits()
				writeCall := submits[len(submits)-1]
				Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
				Expect(writeCall.intent.SubDir).To(Equal(""))

				pathPrefix := destination.Spec.Path + "/"
				expectedWorkloads := []v1alpha1.Workload{
					{Filepath: pathPrefix + "fruit.yaml", Content: "{someApi: foo, someValue: bar}"},
					{Filepath: pathPrefix + "file2.yaml", Content: "{someOtherApi: fooz, someOtherValue: barz}"},
					{
						Filepath: fmt.Sprintf("%s.kratix/%s-%s.yaml", pathPrefix, workPlacement.Namespace, workPlacement.Name),
						Content: `files:
- test-path/fruit.yaml
- test-path/file2.yaml
`,
					},
				}
				writes := decidedWrites(writeCall.intent, map[string][]byte{})
				Expect(writes.ToCreate).To(ConsistOf(expectedWorkloads))
				Expect(writes.ToDelete).To(BeNil())
			})

			It("targets the right state store", func() {
				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				submits := collectSubmits()
				Expect(submits).NotTo(BeEmpty())
				dest := submits[len(submits)-1].dest
				Expect(dest.StateStoreKind).To(Equal("GitStateStore"))
				Expect(dest.StateStoreName).To(Equal("test-state-store"))
				Expect(dest.Branch).To(Equal("main"))
			})

			When("deleting a work placement", func() {
				BeforeEach(func() {
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("submits a delete intent containing only this workplacement's files", func() {
					kratixPath := fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)
					stubStateFileReads(kratixPath, []byte(fmt.Sprintf(`
files:
  - %s/fruit.yaml
  - %s/file2.yaml`, destination.Spec.Path, destination.Spec.Path)))

					Expect(fakeK8sClient.Delete(ctx, &workPlacement)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					deleteCall := submits[len(submits)-1]
					Expect(deleteCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
					writes := decidedWrites(deleteCall.intent, map[string][]byte{})
					Expect(writes.ToDelete).To(ConsistOf(
						fmt.Sprintf("%s/fruit.yaml", destination.Spec.Path),
						fmt.Sprintf("%s/file2.yaml", destination.Spec.Path),
						kratixPath,
					))
				})
			})

			When("statestore and workplacement.spec.workloads has diverged", func() {
				It("only deletes files belonging to this workplacement", func() {
					kratixPath := fmt.Sprintf("%s/.kratix/%s-%s.yaml", destination.Spec.Path, workPlacement.Namespace, workPlacement.Name)
					stubStateFileReads(kratixPath, []byte(`
files:
  - banana.yaml
  - apple.yaml
  - fruit.yaml`))

					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					writeCall := submits[len(submits)-1]
					Expect(writeCall.intent.Reads).To(ConsistOf(kratixPath))
					Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))

					pathPrefix := destination.Spec.Path + "/"
					expectedWorkloads := []v1alpha1.Workload{
						{Filepath: pathPrefix + "fruit.yaml", Content: "{someApi: foo, someValue: bar}"},
						{Filepath: pathPrefix + "file2.yaml", Content: "{someOtherApi: fooz, someOtherValue: barz}"},
						{
							Filepath: fmt.Sprintf("%s.kratix/%s-%s.yaml", pathPrefix, workPlacement.Namespace, workPlacement.Name),
							Content: `files:
- test-path/fruit.yaml
- test-path/file2.yaml
`,
						},
					}
					writes := decidedWrites(writeCall.intent, map[string][]byte{
						kratixPath: []byte(`
files:
  - banana.yaml
  - apple.yaml
  - fruit.yaml`),
					})
					Expect(writes.ToCreate).To(ConsistOf(expectedWorkloads))
					Expect(writes.ToDelete).To(ConsistOf(
						fmt.Sprintf("%s/banana.yaml", destination.Spec.Path),
						fmt.Sprintf("%s/apple.yaml", destination.Spec.Path),
					))
					Expect(writeCall.intent.SubDir).To(Equal(""))
				})
			})
		})

		When("the destination has filepath mode of aggregatedYAML", func() {
			var secondWorkPlacement v1alpha1.WorkPlacement

			BeforeEach(func() {
				destination.Spec.Filepath.Mode = v1alpha1.FilepathModeAggregatedYAML
				destination.Spec.Filepath.Filename = "workloads.yaml"

				setupGitDestination(&gitStateStore, &destination)

				fileContent := `{kratix: is-good}`
				compressedContent, err := compression.CompressContent([]byte(fileContent))
				Expect(err).ToNot(HaveOccurred())

				secondWorkPlacement = createWorkPlacement(workPlacementName+"-2", []v1alpha1.Workload{{
					Filepath: "some-file.yaml",
					Content:  string(compressedContent),
				}})

				Expect(fakeK8sClient.Create(ctx, &secondWorkPlacement)).To(Succeed())

				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))
			})

			It("submits a single aggregated workload at destination.Spec.Path with the user-provided filename", func() {
				submits := collectSubmits()
				Expect(submits).NotTo(BeEmpty())
				writeCall := submits[len(submits)-1]
				Expect(writeCall.intent.SubDir).To(Equal(""))
				Expect(writeCall.intent.WorkPlacement).To(Equal(workPlacement.Name))

				writes := decidedWrites(writeCall.intent, map[string][]byte{})
				Expect(writes.ToCreate).To(Equal([]v1alpha1.Workload{
					{
						Filepath: "test-path/workloads.yaml",
						Content:  "{someApi: foo, someValue: bar}\n---\n{someOtherApi: fooz, someOtherValue: barz}\n---\n{kratix: is-good}",
					},
				}))
				Expect(writes.ToDelete).To(BeEmpty())
			})

			When("the user does not provide a filename", func() {
				BeforeEach(func() {
					Expect(fakeK8sClient.Get(ctx, client.ObjectKeyFromObject(&destination), &destination)).To(Succeed())
					destination.Spec.Filepath.Filename = ""
					Expect(fakeK8sClient.Update(ctx, &destination)).To(Succeed())
					_, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
				})

				It("uses the default filename aggregated.yaml", func() {
					submits := collectSubmits()
					Expect(submits).NotTo(BeEmpty())
					writeCall := submits[len(submits)-1]
					writes := decidedWrites(writeCall.intent, map[string][]byte{})
					Expect(writes.ToCreate).To(HaveLen(1))
					Expect(writes.ToCreate[0].Filepath).To(Equal("test-path/aggregated.yaml"))
				})
			})

			When("one of the workplacements is deleted", func() {
				BeforeEach(func() {
					Expect(fakeK8sClient.Delete(ctx, &workPlacement)).To(Succeed())
				})

				It("removes the workloads of the deleted workplacement from the aggregated file", func() {
					submitCountBeforeDelete := fakeDispatcher.SubmitCallCount()
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					submits := collectSubmits()
					// Since other workplacements remain, the deletion path
					// submits an update intent that rewrites the aggregated
					// file without this workplacement's content; no delete
					// intent is submitted afterwards.
					Expect(len(submits)).To(BeNumerically(">", submitCountBeforeDelete))
					updateCall := submits[len(submits)-1]
					Expect(updateCall.intent.SubDir).To(Equal(""))
					Expect(updateCall.intent.WorkPlacement).To(Equal(workPlacement.Name))

					writes := decidedWrites(updateCall.intent, map[string][]byte{})
					Expect(writes.ToCreate).To(Equal([]v1alpha1.Workload{
						{
							Filepath: "test-path/workloads.yaml",
							Content:  "{kratix: is-good}",
						},
					}))
					Expect(writes.ToDelete).To(BeEmpty())

					Expect(fakeK8sClient.Get(ctx, client.ObjectKey{
						Name:      workPlacement.GetName(),
						Namespace: workPlacement.GetNamespace(),
					}, &workPlacement)).To(HaveOccurred())
				})
			})

			When("all workplacements are deleted", func() {
				BeforeEach(func() {
					Expect(fakeK8sClient.Delete(ctx, &workPlacement)).To(Succeed())
					Expect(fakeK8sClient.Delete(ctx, &secondWorkPlacement)).To(Succeed())
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))
				})

				It("submits a delete intent for the aggregated file", func() {
					submits := collectSubmits()
					Expect(submits).NotTo(BeEmpty())
					deleteCall := submits[len(submits)-1]
					Expect(deleteCall.intent.WorkPlacement).To(Equal(workPlacement.Name))
					writes := decidedWrites(deleteCall.intent, map[string][]byte{})
					Expect(writes.ToDelete).To(ConsistOf("test-path/workloads.yaml"))
				})
			})
		})
	})

	Describe("WorkPlacement Status", func() {
		Context("VersionID", func() {
			BeforeEach(func() {
				setupGitDestination(&gitStateStore, &destination)
			})

			It("is updated with the last VersionID", func() {
				fakeDispatcher.SubmitCalls(func(_ context.Context, _ dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, error) {
					if intent.Decide != nil {
						if _, err := intent.Decide(map[string][]byte{}); err != nil {
							return dispatch.Result{}, err
						}
					}
					return dispatch.Result{VersionID: "an-amazing-version-id"}, nil
				})

				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				updatedWorkPlacement := v1alpha1.WorkPlacement{}
				Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
					Name:      workPlacement.GetName(),
					Namespace: workPlacement.GetNamespace(),
				}, &updatedWorkPlacement)).To(Succeed())
				Expect(updatedWorkPlacement.Status.VersionID).To(Equal("an-amazing-version-id"))
			})

			It("won't update the versionid when no new version is generated", func() {
				workPlacement.Status.VersionID = "an-amazing-version-id"
				Expect(fakeK8sClient.Status().Update(ctx, &workPlacement)).To(Succeed())

				// default stub returns an empty VersionID

				result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).To(Equal(ctrl.Result{}))

				updatedWorkPlacement := v1alpha1.WorkPlacement{}
				Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
					Name:      workPlacement.GetName(),
					Namespace: workPlacement.GetNamespace(),
				}, &updatedWorkPlacement)).To(Succeed())

				Expect(updatedWorkPlacement.Status.VersionID).To(Equal("an-amazing-version-id"))
			})

			When("updating the status fails on updating the versionID", func() {
				It("applies the Version ID on the next X reconciliations", func() {
					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.Name,
						Namespace: workPlacement.Namespace,
					}, &workPlacement)).To(Succeed())
					workPlacement.Status.Conditions = []metav1.Condition{
						{
							Type:    "WriteSucceeded",
							Reason:  "WorkloadsWrittenToStateStore",
							Status:  metav1.ConditionTrue,
							Message: "",
						},
					}
					Expect(fakeK8sClient.Status().Update(ctx, &workPlacement)).To(Succeed())

					errSubResourceUpdate = fmt.Errorf("an-error")
					stubFirstCallVersion(fakeDispatcher, "an-amazing-version-id")

					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).To(MatchError("reconcile loop detected"))
					Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}))

					errSubResourceUpdate = nil
					result, err = t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).ToNot(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					latestWP := v1alpha1.WorkPlacement{}
					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.GetName(),
						Namespace: workPlacement.GetNamespace(),
					}, &latestWP)).To(Succeed())

					Expect(latestWP.Status.VersionID).To(Equal("an-amazing-version-id"))
				})
			})

			When("updating the status fails on updating the conditions", func() {
				It("applies the Version ID on the next reconcile", func() {
					errSubResourceUpdate = fmt.Errorf("an-error")
					stubFirstCallVersion(fakeDispatcher, "an-amazing-version-id")

					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).To(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{RequeueAfter: 15 * time.Second}))

					errSubResourceUpdate = nil

					result, err = t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).ToNot(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					latestWP := v1alpha1.WorkPlacement{}
					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.GetName(),
						Namespace: workPlacement.GetNamespace(),
					}, &latestWP)).To(Succeed())

					Expect(latestWP.Status.VersionID).To(Equal("an-amazing-version-id"))
				})
			})
		})

		Context("Conditions", func() {
			BeforeEach(func() {
				setupGitDestination(&gitStateStore, &destination)
				Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
					Name:      workPlacement.Name,
					Namespace: workPlacement.Namespace,
				}, &workPlacement)).To(Succeed())
				workPlacement.Status.Conditions = nil
				Expect(fakeK8sClient.Update(ctx, &workPlacement)).To(Succeed())
			})

			When("write to statestore has succeeded", func() {
				It("sets WriteSucceeded to true and publishes the right event", func() {
					fakeDispatcher.SubmitCalls(func(_ context.Context, _ dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, error) {
						if intent.Decide != nil {
							if _, err := intent.Decide(map[string][]byte{}); err != nil {
								return dispatch.Result{}, err
							}
						}
						return dispatch.Result{VersionID: "an-id"}, nil
					})
					result, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).NotTo(HaveOccurred())
					Expect(result).To(Equal(ctrl.Result{}))

					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.GetName(),
						Namespace: workPlacement.GetNamespace(),
					}, &workPlacement)).To(Succeed())

					for i := range workPlacement.Status.Conditions {
						workPlacement.Status.Conditions[i].LastTransitionTime = metav1.Time{}
					}

					Expect(workPlacement.Status.Conditions).To(ConsistOf(
						metav1.Condition{
							Type:    "WriteSucceeded",
							Status:  metav1.ConditionTrue,
							Message: "Workloads written to State Store",
							Reason:  "WorkloadsWrittenToStateStore"},
						metav1.Condition{
							Type:    "Ready",
							Status:  metav1.ConditionTrue,
							Reason:  "WorkloadsWrittenToTargetDestination",
							Message: "Ready"}))

					Eventually(workplacementRecorder.Events).Should(Receive(ContainSubstring(
						"successfully written to Destination: test-destination with versionID: an-id")))
				})
			})

			When("write to statestore has failed", func() {
				It("sets WriteSucceeded to false and publishes the right event", func() {
					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.Name,
						Namespace: workPlacement.Namespace,
					}, &workPlacement)).To(Succeed())
					workPlacement.Status.Conditions = nil
					Expect(fakeK8sClient.Update(ctx, &workPlacement)).To(Succeed())

					fakeDispatcher.SubmitReturns(dispatch.Result{}, fmt.Errorf("whatever error"))
					_, err := t.reconcileUntilCompletion(reconciler, &workPlacement)
					Expect(err).To(HaveOccurred())

					Expect(fakeK8sClient.Get(ctx, types.NamespacedName{
						Name:      workPlacement.GetName(),
						Namespace: workPlacement.GetNamespace(),
					}, &workPlacement)).To(Succeed())
					for i := range workPlacement.Status.Conditions {
						workPlacement.Status.Conditions[i].LastTransitionTime = metav1.Time{}
					}
					Expect(workPlacement.Status.Conditions).To(ConsistOf(
						metav1.Condition{
							Type:    "WriteSucceeded",
							Status:  metav1.ConditionFalse,
							Reason:  "WorkloadsFailedWrite",
							Message: "whatever error"},
						metav1.Condition{
							Type:    "Ready",
							Status:  metav1.ConditionFalse,
							Reason:  "WorkloadsFailedWrite",
							Message: "Failing"}))
					Eventually(workplacementRecorder.Events).Should(Receive(ContainSubstring(
						"error writing to destination; check kubectl get destination for more info: whatever error")))
				})
			})
		})

	})
})

// stubFirstCallVersion makes the first Submit return the given VersionID,
// and subsequent submits return an empty VersionID. Mirrors the previous
// fakeWriter.UpdateFilesReturnsOnCall(0, "version", nil) pattern.
func stubFirstCallVersion(fake *dispatchfakes.FakeDispatcher, versionID string) {
	var calls int
	fake.SubmitCalls(func(_ context.Context, _ dispatch.DestinationKey, intent dispatch.Intent) (dispatch.Result, error) {
		if intent.Decide != nil {
			if _, err := intent.Decide(map[string][]byte{}); err != nil {
				return dispatch.Result{}, err
			}
		}
		defer func() { calls++ }()
		if calls == 0 {
			return dispatch.Result{VersionID: versionID}, nil
		}
		return dispatch.Result{}, nil
	})
}

func setupGitDestination(gitStateStore *v1alpha1.GitStateStore, destination *v1alpha1.Destination) {
	Expect(fakeK8sClient.Create(ctx, &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "metav1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-secret",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("test-username"),
			"password": []byte("test-password"),
		},
	})).To(Succeed())
	*gitStateStore = v1alpha1.GitStateStore{
		TypeMeta: metav1.TypeMeta{
			Kind:       "GitStateStore",
			APIVersion: "platform.kratix.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-state-store",
		},
		Spec: v1alpha1.GitStateStoreSpec{
			StateStoreCoreFields: v1alpha1.StateStoreCoreFields{
				SecretRef: &corev1.SecretReference{
					Name:      "test-secret",
					Namespace: "default",
				},
			},
			URL:        "",
			Branch:     "main",
			AuthMethod: v1alpha1.BasicAuthMethod,
		},
	}
	Expect(fakeK8sClient.Create(ctx, gitStateStore)).To(Succeed())
	destination.Spec.StateStoreRef.Kind = "GitStateStore"
	destination.Spec.StateStoreRef.Name = "test-state-store"

	Expect(fakeK8sClient.Create(ctx, destination)).To(Succeed())
}

func createWorkPlacement(name string, workload []v1alpha1.Workload) v1alpha1.WorkPlacement {
	return v1alpha1.WorkPlacement{
		TypeMeta: metav1.TypeMeta{
			Kind:       "WorkPlacement",
			APIVersion: "platform.kratix.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				controller.TargetDestinationNameLabel: "test-destination",
			},
		},
		Spec: v1alpha1.WorkPlacementSpec{
			TargetDestinationName: "test-destination",
			ID:                    hash.ComputeHash("."),
			Workloads:             workload,
			PromiseName:           "test-promise",
			ResourceName:          "test-resource",
		},
	}
}

func setupTestMeterProvider() (*sdkmetric.ManualReader, func()) {
	reader := sdkmetric.NewManualReader()
	original := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	return reader, func() {
		otel.SetMeterProvider(original)
	}
}

func collectWorkPlacementWriteMetrics(ctx context.Context, reader *sdkmetric.ManualReader) map[string]int64 {
	var rm metricdata.ResourceMetrics
	Expect(reader.Collect(ctx, &rm)).To(Succeed())

	counts := make(map[string]int64)
	for _, scopeMetrics := range rm.ScopeMetrics {
		for _, metricData := range scopeMetrics.Metrics {
			if metricData.Name != telemetry.WorkPlacementWritesMetric {
				continue
			}

			sum, ok := metricData.Data.(metricdata.Sum[int64])
			Expect(ok).To(BeTrue(), "expected WorkPlacement write metric to be an int64 Sum")

			for _, dataPoint := range sum.DataPoints {
				resultAttr, ok := dataPoint.Attributes.Value(attribute.Key("result"))
				Expect(ok).To(BeTrue(), "expected WorkPlacement write metric to include result attribute")
				counts[resultAttr.AsString()] = dataPoint.Value
			}
		}
	}

	return counts
}
