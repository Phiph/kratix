package dispatch_test

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	clocktesting "k8s.io/utils/clock/testing"

	"github.com/syntasso/kratix/api/v1alpha1"
	"github.com/syntasso/kratix/lib/writers/dispatch"
	"github.com/syntasso/kratix/lib/writers/dispatch/dispatchfakes"
)

var _ = Describe("Worker", func() {
	var (
		fakeClock   *clocktesting.FakeClock
		fakeBackend *dispatchfakes.FakeBackend
		dest        dispatch.DestinationKey
		cfg         dispatch.DispatcherConfig
	)

	BeforeEach(func() {
		fakeClock = clocktesting.NewFakeClock(time.Unix(0, 0))
		fakeBackend = &dispatchfakes.FakeBackend{}
		dest = dispatch.DestinationKey{
			StateStoreKind: "GitStateStore",
			StateStoreName: "test-store",
			Branch:         "main",
			Path:           "dest-a",
		}
		cfg = dispatch.DispatcherConfig{
			BatchWindow:  100 * time.Millisecond,
			BatchMaxSize: 100,
			Clock:        fakeClock,
			Logger:       logr.Discard(),
		}
		fakeBackend.ReadReturns(map[string][]byte{}, nil)
		fakeBackend.ApplyBatchReturns(dispatch.BatchResult{
			VersionID: "sha-1",
			PerIntent: map[string]error{},
		})
	})

	It("fires a batch after the configured window when one intent is submitted", func() {
		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		intent := dispatch.Intent{
			WorkPlacement: "wp-1",
			SubDir:        "sub",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				return dispatch.Writes{
					ToCreate: []v1alpha1.Workload{{Filepath: "a.yaml", Content: "a"}},
				}, nil
			},
		}

		resultCh := make(chan dispatch.SubmitResult, 1)
		Expect(w.Submit(context.Background(), intent, resultCh)).To(Succeed())

		// Worker should be waiting in the batch window. Advance clock past it.
		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(150 * time.Millisecond)

		var got dispatch.SubmitResult
		Eventually(resultCh).Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Result.VersionID).To(Equal("sha-1"))
		Expect(fakeBackend.ApplyBatchCallCount()).To(Equal(1))

		_, batch := fakeBackend.ApplyBatchArgsForCall(0)
		Expect(batch).To(HaveLen(1))
		Expect(batch[0].WorkPlacement).To(Equal("wp-1"))
		Expect(batch[0].SubDir).To(Equal("sub"))
		Expect(batch[0].Writes.ToCreate).To(HaveLen(1))
	})
})
