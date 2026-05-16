package dispatch_test

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers/dispatch"
	"github.com/syntasso/kratix/lib/writers/dispatch/dispatchfakes"
)

var _ = Describe("Dispatcher.Submit", func() {
	var (
		fakeClock       *clocktesting.FakeClock
		gitFake, s3Fake *dispatchfakes.FakeBackend
		gitCallCount    int
		gitDest, s3Dest dispatch.DestinationKey
		cfg             dispatch.DispatcherConfig
	)

	BeforeEach(func() {
		fakeClock = clocktesting.NewFakeClock(time.Unix(0, 0))
		gitFake = &dispatchfakes.FakeBackend{}
		s3Fake = &dispatchfakes.FakeBackend{}
		gitFake.ApplyBatchReturns(dispatch.BatchResult{VersionID: "git-sha"})
		s3Fake.ApplyBatchReturns(dispatch.BatchResult{VersionID: "s3-id"})
		gitCallCount = 0

		gitDest = dispatch.DestinationKey{StateStoreKind: "GitStateStore", StateStoreName: "g", Branch: "main", Path: "p"}
		s3Dest = dispatch.DestinationKey{StateStoreKind: "BucketStateStore", StateStoreName: "b", Path: "p"}

		cfg = dispatch.DispatcherConfig{
			BatchWindow:  100 * time.Millisecond,
			BatchMaxSize: 100,
			Clock:        fakeClock,
			Logger:       logr.Discard(),
			NewGitBackend: func(_ logr.Logger, _ dispatch.DestinationKey, _ v1alpha1.GitStateStoreSpec, _ map[string][]byte) (dispatch.Backend, error) {
				gitCallCount++
				return gitFake, nil
			},
			NewS3Backend: func(_ logr.Logger, _ dispatch.DestinationKey, _ v1alpha1.BucketStateStoreSpec, _ map[string][]byte) (dispatch.Backend, error) {
				return s3Fake, nil
			},
		}
	})

	It("constructs a worker on first Submit for a destination and reuses it for the second", func() {
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())

		Expect(d.RegisterGitDestination(gitDest, v1alpha1.GitStateStoreSpec{Branch: "main"}, nil)).To(Succeed())

		intent := dispatch.Intent{
			WorkPlacement: "wp",
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}

		done1 := make(chan dispatch.SubmitResult, 1)
		go func() {
			r, err := d.Submit(context.Background(), gitDest, intent)
			done1 <- dispatch.SubmitResult{Result: r, Err: err}
		}()
		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)
		Eventually(done1).Should(Receive())

		done2 := make(chan dispatch.SubmitResult, 1)
		go func() {
			r, err := d.Submit(context.Background(), gitDest, intent)
			done2 <- dispatch.SubmitResult{Result: r, Err: err}
		}()
		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)
		Eventually(done2).Should(Receive())

		Expect(gitCallCount).To(Equal(1), "backend factory should be called only once")
	})

	It("returns an error from Submit if the destination has not been registered", func() {
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())

		_, err := d.Submit(context.Background(), gitDest, dispatch.Intent{
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		})
		Expect(err).To(HaveOccurred())
	})

	It("routes intents to the correct worker per destination", func() {
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())

		Expect(d.RegisterGitDestination(gitDest, v1alpha1.GitStateStoreSpec{Branch: "main"}, nil)).To(Succeed())
		Expect(d.RegisterS3Destination(s3Dest, v1alpha1.BucketStateStoreSpec{}, nil)).To(Succeed())

		intent := dispatch.Intent{
			WorkPlacement: "wp",
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}

		gitDone := make(chan dispatch.SubmitResult, 1)
		s3Done := make(chan dispatch.SubmitResult, 1)
		go func() {
			r, err := d.Submit(context.Background(), gitDest, intent)
			gitDone <- dispatch.SubmitResult{Result: r, Err: err}
		}()
		go func() {
			r, err := d.Submit(context.Background(), s3Dest, intent)
			s3Done <- dispatch.SubmitResult{Result: r, Err: err}
		}()
		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(gitDone).Should(Receive(&got))
		Expect(got.Result.VersionID).To(Equal("git-sha"))
		Eventually(s3Done).Should(Receive(&got))
		Expect(got.Result.VersionID).To(Equal("s3-id"))
	})
})

var _ = Describe("Dispatcher.Cleanup delivery", func() {
	It("delivers ErrDestinationGone to pending intents when Cleanup is called", func() {
		fakeClock := clocktesting.NewFakeClock(time.Unix(0, 0))
		fakeBackend := &dispatchfakes.FakeBackend{}
		fakeBackend.ApplyBatchReturns(dispatch.BatchResult{VersionID: "sha"})

		key := dispatch.DestinationKey{StateStoreKind: "GitStateStore", StateStoreName: "g", Branch: "main"}
		cfg := dispatch.DispatcherConfig{
			BatchWindow:  10 * time.Second, // long enough that the intent sits in pending
			BatchMaxSize: 100,
			Clock:        fakeClock,
			Logger:       logr.Discard(),
			NewGitBackend: func(_ logr.Logger, _ dispatch.DestinationKey, _ v1alpha1.GitStateStoreSpec, _ map[string][]byte) (dispatch.Backend, error) {
				return fakeBackend, nil
			},
		}
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())
		Expect(d.RegisterGitDestination(key, v1alpha1.GitStateStoreSpec{}, nil)).To(Succeed())

		submitResult := make(chan error, 1)
		go func() {
			_, err := d.Submit(context.Background(), key, dispatch.Intent{
				WorkPlacement: "wp",
				Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
			})
			submitResult <- err
		}()
		// Wait for the worker to pick up the intent (it's now waiting in the batch window).
		Eventually(fakeClock.HasWaiters).Should(BeTrue())

		Expect(d.Cleanup(key)).To(Succeed())

		var err error
		Eventually(submitResult).Should(Receive(&err))
		Expect(err).To(MatchError(dispatch.ErrDestinationGone))
	})
})

var _ = Describe("Dispatcher misc", func() {
	It("can be constructed and Shutdown without ever doing work", func() {
		d := dispatch.NewDispatcher(dispatch.DispatcherConfig{Logger: logr.Discard()})
		Expect(d.Shutdown(context.Background())).To(Succeed())
	})
})

var _ = Describe("Dispatcher.Validate", func() {
	var (
		validateFake *dispatchfakes.FakeBackend
		cfg          dispatch.DispatcherConfig
		key          dispatch.DestinationKey
	)

	BeforeEach(func() {
		validateFake = &dispatchfakes.FakeBackend{}
		key = dispatch.DestinationKey{StateStoreKind: "GitStateStore", StateStoreName: "g", Branch: "main"}
		cfg = dispatch.DispatcherConfig{
			Logger: logr.Discard(),
			NewGitBackend: func(_ logr.Logger, _ dispatch.DestinationKey, _ v1alpha1.GitStateStoreSpec, _ map[string][]byte) (dispatch.Backend, error) {
				return validateFake, nil
			},
		}
	})

	It("constructs a fresh backend, calls Validate, and Closes it; does not create a worker", func() {
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())
		Expect(d.RegisterGitDestination(key, v1alpha1.GitStateStoreSpec{Branch: "main"}, nil)).To(Succeed())

		Expect(d.Validate(context.Background(), key)).To(Succeed())
		Expect(validateFake.ValidateCallCount()).To(Equal(1))
		Expect(validateFake.CloseCallCount()).To(Equal(1))
	})

	It("returns the underlying error when Validate fails", func() {
		validateFake.ValidateReturns(errors.New("bad creds"))
		d := dispatch.NewDispatcher(cfg)
		defer d.Shutdown(context.Background())
		Expect(d.RegisterGitDestination(key, v1alpha1.GitStateStoreSpec{Branch: "main"}, nil)).To(Succeed())

		err := d.Validate(context.Background(), key)
		Expect(err).To(MatchError("bad creds"))
		Expect(validateFake.CloseCallCount()).To(Equal(1))
	})
})
