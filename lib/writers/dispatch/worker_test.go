package dispatch_test

import (
	"context"
	"errors"
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

	It("quarantines an intent whose Decide returns an error and applies the rest", func() {
		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		wantErr := errors.New("bad intent")
		chGood := make(chan dispatch.SubmitResult, 1)
		chBad := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-good",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				return dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: "ok.yaml"}}}, nil
			},
		}, chGood)).To(Succeed())

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-bad",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				return dispatch.Writes{}, wantErr
			},
		}, chBad)).To(Succeed())

		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(chBad).Should(Receive(&got))
		Expect(got.Err).To(MatchError(wantErr))

		Eventually(chGood).Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())

		Expect(fakeBackend.ApplyBatchCallCount()).To(Equal(1))
		_, batch := fakeBackend.ApplyBatchArgsForCall(0)
		Expect(batch).To(HaveLen(1))
		Expect(batch[0].WorkPlacement).To(Equal("wp-good"))
	})

	It("fails every intent in the batch when backend.Read fails", func() {
		wantErr := errors.New("read exploded")
		fakeBackend.ReadReturns(nil, wantErr)

		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		chA := make(chan dispatch.SubmitResult, 1)
		chB := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-a",
			Reads:         []string{"foo.yaml"},
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chA)).To(Succeed())
		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-b",
			Reads:         []string{"foo.yaml"},
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chB)).To(Succeed())

		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(chA).Should(Receive(&got))
		Expect(got.Err).To(MatchError(wantErr))
		Eventually(chB).Should(Receive(&got))
		Expect(got.Err).To(MatchError(wantErr))

		Expect(fakeBackend.ApplyBatchCallCount()).To(BeZero())
	})

	It("delivers per-intent errors when ApplyBatch returns them", func() {
		fakeBackend.ApplyBatchReturns(dispatch.BatchResult{
			VersionID: "sha-2",
			PerIntent: map[string]error{
				"wp-a|sub": nil,
				"wp-b|sub": errors.New("just b broke"),
			},
		})

		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		chA := make(chan dispatch.SubmitResult, 1)
		chB := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-a", SubDir: "sub",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chA)).To(Succeed())
		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-b", SubDir: "sub",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chB)).To(Succeed())

		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(chA).Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())
		Expect(got.Result.VersionID).To(Equal("sha-2"))

		Eventually(chB).Should(Receive(&got))
		Expect(got.Err).To(MatchError(ContainSubstring("just b broke")))
	})

	It("times out a slow Decide and quarantines only that intent", func() {
		// This spec uses a real clock because Decide's timeout context is wall-time.
		// The other specs in this describe use a FakeClock for batch-window timing.
		realClockCfg := dispatch.DispatcherConfig{
			BatchWindow:   50 * time.Millisecond,
			BatchMaxSize:  100,
			DecideTimeout: 50 * time.Millisecond,
			Logger:        logr.Discard(),
		}
		w := dispatch.NewWorker(dest, fakeBackend, realClockCfg)
		defer w.Stop()

		chSlow := make(chan dispatch.SubmitResult, 1)
		chFast := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-slow",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				time.Sleep(500 * time.Millisecond)
				return dispatch.Writes{}, nil
			},
		}, chSlow)).To(Succeed())
		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-fast",
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chFast)).To(Succeed())

		var got dispatch.SubmitResult
		Eventually(chSlow, "2s").Should(Receive(&got))
		Expect(got.Err).To(MatchError(context.DeadlineExceeded))

		Eventually(chFast, "2s").Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())
	})

	It("recovers from a panic in Decide and surfaces ErrBatchFailed to that caller", func() {
		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		chPanic := make(chan dispatch.SubmitResult, 1)
		chGood := make(chan dispatch.SubmitResult, 1)

		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-panic",
			Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
				panic("kaboom")
			},
		}, chPanic)).To(Succeed())
		Expect(w.Submit(context.Background(), dispatch.Intent{
			WorkPlacement: "wp-good",
			Decide:        func(_ map[string][]byte) (dispatch.Writes, error) { return dispatch.Writes{}, nil },
		}, chGood)).To(Succeed())

		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(chPanic).Should(Receive(&got))
		Expect(got.Err).To(MatchError(dispatch.ErrBatchFailed))

		Eventually(chGood).Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())
	})

	It("dedups intents by (WorkPlacement, SubDir); older intent gets ErrSuperseded", func() {
		w := dispatch.NewWorker(dest, fakeBackend, cfg)
		defer w.Stop()

		chOld := make(chan dispatch.SubmitResult, 1)
		chNew := make(chan dispatch.SubmitResult, 1)

		makeIntent := func(label string) dispatch.Intent {
			return dispatch.Intent{
				WorkPlacement: "wp-same",
				SubDir:        "same-sub",
				Decide: func(_ map[string][]byte) (dispatch.Writes, error) {
					return dispatch.Writes{ToCreate: []v1alpha1.Workload{{Filepath: label}}}, nil
				},
			}
		}

		Expect(w.Submit(context.Background(), makeIntent("old.yaml"), chOld)).To(Succeed())
		// Allow the first intent to be picked up before the second arrives.
		Eventually(fakeClock.HasWaiters).Should(BeTrue())
		Expect(w.Submit(context.Background(), makeIntent("new.yaml"), chNew)).To(Succeed())

		fakeClock.Step(2 * cfg.BatchWindow)

		var got dispatch.SubmitResult
		Eventually(chOld).Should(Receive(&got))
		Expect(got.Err).To(MatchError(dispatch.ErrSuperseded))

		Eventually(chNew).Should(Receive(&got))
		Expect(got.Err).NotTo(HaveOccurred())

		Expect(fakeBackend.ApplyBatchCallCount()).To(Equal(1))
		_, batch := fakeBackend.ApplyBatchArgsForCall(0)
		Expect(batch).To(HaveLen(1))
		Expect(batch[0].Writes.ToCreate[0].Filepath).To(Equal("new.yaml"))
	})
})
