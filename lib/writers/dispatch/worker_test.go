package dispatch_test

import (
	"context"
	"fmt"
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

	It("fires a batch immediately when BatchMaxSize is reached without waiting for the window", func() {
		cfg.BatchMaxSize = 3
		cfg.BatchWindow = 1 * time.Hour // would never elapse during the test
		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		resultChs := make([]chan dispatch.SubmitResult, 3)
		for i := range resultChs {
			resultChs[i] = make(chan dispatch.SubmitResult, 1)
			intent := dispatch.Intent{
				WorkPlacement: "wp",
				SubDir:        fmt.Sprintf("sub-%d", i),
				Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
					return dispatch.Writes{}, nil
				},
			}
			Expect(w.Submit(context.Background(), intent, resultChs[i])).To(Succeed())
		}

		// All three resultChs should receive without us advancing the clock.
		for i := range resultChs {
			Eventually(resultChs[i]).Should(Receive())
			_ = i
		}
		Expect(fakeBackend.ApplyBatchCallCount()).To(Equal(1))
		_, batch := fakeBackend.ApplyBatchArgsForCall(0)
		Expect(batch).To(HaveLen(3))
	})

	It("collects unique reads across all intents and passes them to Decide", func() {
		fakeBackend.ReadReturns(map[string][]byte{
			"shared.yaml":   []byte("shared"),
			"intent-a.yaml": []byte("a"),
			"intent-b.yaml": []byte("b"),
		}, nil)

		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		var capturedA, capturedB map[string][]byte
		ch1 := make(chan dispatch.SubmitResult, 1)
		ch2 := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-a",
			SubDir:        "sub-a",
			Reads:         []string{"shared.yaml", "intent-a.yaml"},
			Decide: func(reads map[string][]byte) (dispatch.Writes, error) {
				capturedA = reads
				return dispatch.Writes{}, nil
			},
		}, ch1)).To(Succeed())

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-b",
			SubDir:        "sub-b",
			Reads:         []string{"shared.yaml", "intent-b.yaml"},
			Decide: func(reads map[string][]byte) (dispatch.Writes, error) {
				capturedB = reads
				return dispatch.Writes{}, nil
			},
		}, ch2)).To(Succeed())

		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		Eventually(ch1).Should(Receive())
		Eventually(ch2).Should(Receive())

		Expect(fakeBackend.ReadCallCount()).To(Equal(1))
		_, paths := fakeBackend.ReadArgsForCall(0)
		Expect(paths).To(ConsistOf("shared.yaml", "intent-a.yaml", "intent-b.yaml"))

		Expect(capturedA).To(HaveKeyWithValue("shared.yaml", []byte("shared")))
		Expect(capturedA).To(HaveKeyWithValue("intent-a.yaml", []byte("a")))
		Expect(capturedA).NotTo(HaveKey("intent-b.yaml"))

		Expect(capturedB).To(HaveKeyWithValue("shared.yaml", []byte("shared")))
		Expect(capturedB).To(HaveKeyWithValue("intent-b.yaml", []byte("b")))
		Expect(capturedB).NotTo(HaveKey("intent-a.yaml"))
	})
})
