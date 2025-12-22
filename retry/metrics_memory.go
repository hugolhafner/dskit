package retry

import (
	"context"
	"sync/atomic"
	"time"
)

type InMemoryMetrics struct {
	attemptsTotal         atomic.Int64
	attemptsSuccess       atomic.Int64
	attemptsFailure       atomic.Int64
	attemptsDurationTotal atomic.Int64

	outcomeTotal         atomic.Int64
	outcomeSuccess       atomic.Int64
	outcomeFailure       atomic.Int64
	outcomeDurationTotal atomic.Int64

	backoffDurationTotal atomic.Int64
}

var _ Metrics = (*InMemoryMetrics)(nil)

func NewInMemoryMetrics() *InMemoryMetrics {
	return &InMemoryMetrics{}
}

func (m *InMemoryMetrics) RecordAttempt(_ context.Context, attempt Attempt) {
	m.attemptsTotal.Add(1)
	if attempt.IsSuccess() {
		m.attemptsSuccess.Add(1)
	} else {
		m.attemptsFailure.Add(1)
	}
	m.attemptsDurationTotal.Add(attempt.Duration.Milliseconds())
}

func (m *InMemoryMetrics) RecordOutcome(_ context.Context, outcome Outcome) {
	m.outcomeTotal.Add(1)
	if outcome.IsSuccess() {
		m.outcomeSuccess.Add(1)
	} else {
		m.outcomeFailure.Add(1)
	}
	m.outcomeDurationTotal.Add(outcome.TotalDuration.Milliseconds())
}

func (m *InMemoryMetrics) RecordBackoff(_ context.Context, _ string, _ int, duration time.Duration) {
	m.backoffDurationTotal.Add(duration.Milliseconds())
}

func (m *InMemoryMetrics) GetMetrics() map[string]int64 {
	return map[string]int64{
		"attempts_total":          m.attemptsTotal.Load(),
		"attempts_success":        m.attemptsSuccess.Load(),
		"attempts_failure":        m.attemptsFailure.Load(),
		"attempts_duration_total": m.attemptsDurationTotal.Load(),
		"outcome_total":           m.outcomeTotal.Load(),
		"outcome_success":         m.outcomeSuccess.Load(),
		"outcome_failure":         m.outcomeFailure.Load(),
		"outcome_duration_total":  m.outcomeDurationTotal.Load(),
		"backoff_duration_total":  m.backoffDurationTotal.Load(),
	}
}
