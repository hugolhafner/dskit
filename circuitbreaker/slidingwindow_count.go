package circuitbreaker

import (
	"container/ring"
)

var _ Window = (*CountWindow)(nil)

type CountWindow struct {
	ring *ring.Ring

	successCount  int32
	failureCount  int32
	slowCallCount int32
}

func NewCountWindow(size int) *CountWindow {
	return &CountWindow{
		ring: ring.New(size),
	}
}

func (w *CountWindow) RecordOutcome(outcome CallOutcome) {
	oldOutcome, ok := w.ring.Value.(CallOutcome)
	if ok {
		w.decrementOutcome(oldOutcome)
	}

	w.ring.Value = outcome
	w.incrementOutcome(outcome)
	w.ring = w.ring.Next()
}

func (w *CountWindow) decrementOutcome(outcome CallOutcome) {
	switch outcome {
	case OutcomeSuccess:
		w.successCount--
	case OutcomeFailure:
		w.failureCount--
	case OutcomeSlowSuccess:
		w.successCount--
		w.slowCallCount--
	case OutcomeSlowFailure:
		w.failureCount--
		w.slowCallCount--
	}
}

func (w *CountWindow) incrementOutcome(outcome CallOutcome) {
	switch outcome {
	case OutcomeSuccess:
		w.successCount++
	case OutcomeFailure:
		w.failureCount++
	case OutcomeSlowSuccess:
		w.successCount++
		w.slowCallCount++
	case OutcomeSlowFailure:
		w.failureCount++
		w.slowCallCount++
	}
}

func (w *CountWindow) Size() int {
	return int(w.successCount + w.failureCount)
}

func (w *CountWindow) Reset() {
	w.ring = ring.New(w.ring.Len())
	w.successCount = 0
	w.failureCount = 0
	w.slowCallCount = 0
}

func (w *CountWindow) CallRates() (int, float64, float64, float64) {
	totalCalls := w.Size()
	if totalCalls == 0 {
		return 0, 0, 0, 0
	}

	successRate := (float64(w.successCount) / float64(totalCalls)) * 100
	failureRate := (float64(w.failureCount) / float64(totalCalls)) * 100
	slowCallRate := (float64(w.slowCallCount) / float64(totalCalls)) * 100

	return totalCalls, successRate, failureRate, slowCallRate
}
