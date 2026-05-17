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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/internal/controller"
	"github.com/syntasso/kratix/lib/writers/dispatch"
	"github.com/syntasso/kratix/lib/writers/dispatch/dispatchfakes"
)

var _ = Describe("BucketStateStore Controller", func() {
	var (
		bucketStateStore         *v1alpha1.BucketStateStore
		updatedBucketStateStore  *v1alpha1.BucketStateStore
		reconciler               *controller.BucketStateStoreReconciler
		fakeDispatcher           *dispatchfakes.FakeDispatcher
		eventRecorder            *record.FakeRecorder
		secret                   *corev1.Secret
		ctx                      context.Context
		testBucketStateStoreName types.NamespacedName
		secretName               string
	)

	BeforeEach(func() {
		ctx = context.Background()

		testBucketStateStoreName = types.NamespacedName{
			Name: "default-store",
		}

		secretName = "store-secret"
		eventRecorder = record.NewFakeRecorder(1024)

		fakeDispatcher = &dispatchfakes.FakeDispatcher{}
		fakeDispatcher.RegisterS3DestinationReturns(nil)
		fakeDispatcher.ValidateReturns(nil)
		fakeDispatcher.CleanupReturns(nil)

		reconciler = &controller.BucketStateStoreReconciler{
			Client:          fakeK8sClient,
			Scheme:          scheme.Scheme,
			Log:             ctrl.Log.WithName("controllers").WithName("BucketStateStore"),
			EventRecorder:   eventRecorder,
			RepositoryCache: controller.NewRepositoryCache(),
			Dispatcher:      fakeDispatcher,
		}

		bucketStateStore = &v1alpha1.BucketStateStore{
			ObjectMeta: metav1.ObjectMeta{
				Name: "default-store",
			},
			TypeMeta: metav1.TypeMeta{
				Kind:       "BucketStateStore",
				APIVersion: "platform.kratix.io/v1alpha1",
			},
			Spec: v1alpha1.BucketStateStoreSpec{
				BucketName: "default-store",
				Endpoint:   "localhost:3000",
				StateStoreCoreFields: v1alpha1.StateStoreCoreFields{
					SecretRef: &corev1.SecretReference{
						Name:      secretName,
						Namespace: "default",
					},
				},
			},
		}

		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: "default",
			},
			TypeMeta: metav1.TypeMeta{
				Kind:       "Secret",
				APIVersion: "v1",
			},
			Data: map[string][]byte{
				"accessKeyID":     []byte("my-access-key"),
				"secretAccessKey": []byte("my-secret-access-key"),
			},
		}

		updatedBucketStateStore = &v1alpha1.BucketStateStore{}
	})

	When("the BucketStateStore does not exist", func() {
		It("reconciles without error and does not requeue", func() {
			result, err := t.reconcileUntilCompletion(reconciler, bucketStateStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))
		})
	})

	When("the BucketStateStore exists", func() {
		var result ctrl.Result
		var err error

		BeforeEach(func() {
			Expect(fakeK8sClient.Create(ctx, bucketStateStore)).To(Succeed())
			Expect(fakeK8sClient.Create(ctx, secret)).To(Succeed())
		})

		It("reconciles without error and does not requeue", func() {
			result, err := t.reconcileUntilCompletion(reconciler, bucketStateStore)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal(ctrl.Result{}))

			By("registering the destination with the dispatcher", func() {
				Expect(fakeDispatcher.RegisterS3DestinationCallCount()).To(Equal(2))
				key, _, _ := fakeDispatcher.RegisterS3DestinationArgsForCall(0)
				Expect(key).To(Equal(dispatch.DestinationKey{
					StateStoreKind: "BucketStateStore",
					StateStoreName: "default-store",
				}))
			})

			By("validating permissions on the state store", func() {
				Expect(fakeDispatcher.ValidateCallCount()).To(Equal(2))
			})

			By("updating the status to say the state store is ready", func() {
				Expect(fakeK8sClient.Get(ctx, testBucketStateStoreName, updatedBucketStateStore)).To(Succeed())
				Expect(updatedBucketStateStore.Status.Status).To(Equal(controller.StatusReady))
				Expect(updatedBucketStateStore.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", "Ready"),
					HaveField("Message", "State store is ready"),
					HaveField("Reason", "StateStoreReady"),
					HaveField("Status", metav1.ConditionTrue),
				)))
			})

			By("firing an event to indicate the state store is ready", func() {
				Eventually(eventRecorder.Events).Should(Receive(ContainSubstring(
					"Normal Ready BucketStateStore \"default-store\" is ready")))
			})
		})

		When("registering the destination fails", func() {
			BeforeEach(func() {
				bucketStateStore.Status.Status = controller.StatusReady
				Expect(fakeK8sClient.Status().Update(ctx, bucketStateStore)).To(Succeed())

				fakeDispatcher.RegisterS3DestinationReturns(errors.New("unable to create bucket writer: writer-create-error"))

				result, err = t.reconcileUntilCompletion(reconciler, bucketStateStore, &opts{requeueExpected: true})
			})

			It("updates the status ", func() {
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Second * 15}))
				Expect(err).ToNot(HaveOccurred())

				Expect(fakeK8sClient.Get(ctx, testBucketStateStoreName, updatedBucketStateStore)).To(Succeed())
				Expect(updatedBucketStateStore.Status.Status).To(Equal(controller.StatusNotReady))
				Expect(updatedBucketStateStore.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", "Ready"),
					HaveField("Message", "unable to create bucket writer: writer-create-error"),
					HaveField("Reason", "ErrorInitialisingWriter"),
					HaveField("Status", metav1.ConditionFalse),
				)))
			})

			It("fires an event to indicate writer initialization failed", func() {
				Eventually(eventRecorder.Events).Should(Receive(ContainSubstring(
					"Warning NotReady unable to create bucket writer: writer-create-error")))
			})
		})

		When("the dispatcher fails to validate permissions", func() {
			BeforeEach(func() {
				fakeDispatcher.ValidateReturns(errors.New("ARGH!"))

				bucketStateStore.Status.Status = controller.StatusReady
				Expect(fakeK8sClient.Status().Update(ctx, bucketStateStore)).To(Succeed())

				result, err = t.reconcileUntilCompletion(reconciler, bucketStateStore)
			})

			It("updates the status to say permissions validation failed", func() {
				Expect(err).To(MatchError(ContainSubstring("reconcile loop detected")))
				Expect(result).To(Equal(ctrl.Result{RequeueAfter: time.Second * 15}))

				Expect(fakeK8sClient.Get(ctx, testBucketStateStoreName, updatedBucketStateStore)).To(Succeed())
				Expect(updatedBucketStateStore.Status.Status).To(Equal(controller.StatusNotReady))
				Expect(updatedBucketStateStore.Status.Conditions).To(ContainElement(SatisfyAll(
					HaveField("Type", "Ready"),
					HaveField("Message", "Error validating state store permissions: ARGH!"),
					HaveField("Reason", "ErrorValidatingPermissions"),
					HaveField("Status", metav1.ConditionFalse),
				)))
			})

			It("fires an event to indicate permissions validation failed", func() {
				Eventually(eventRecorder.Events).Should(Receive(ContainSubstring(
					"Warning NotReady Error validating state store permissions: ARGH!")))
			})
		})
	})
})
