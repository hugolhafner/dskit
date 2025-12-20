package retry

import (
	"context"
	"sync/atomic"
	"time"
)

var _ Metrics = (*NoopMetrics)(nil)

var _globalMetrics = atomic.Value{}

// OutcomeStatus represents the final status of a retry sequence
type OutcomeStatus string

const (
	OutcomeStatusSuccess OutcomeStatus = "success"
	OutcomeStatusError   OutcomeStatus = "error"
)

type OutcomeFailureReason string

const (
	OutcomeFailureReasonExhausted    OutcomeFailureReason = "exhausted"
	OutcomeFailureReasonTimeout      OutcomeFailureReason = "timeout"
	OutcomeFailureReasonCanceled     OutcomeFailureReason = "canceled"
	OutcomeFailureReasonNonRetryable OutcomeFailureReason = "non_retryable"
)

// AttemptStatus represents the status of a single attempt
type AttemptStatus string

const (
	AttemptStatusSuccess AttemptStatus = "success"
	AttemptStatusError   AttemptStatus = "error"
)

// AttemptFailureReason represents the reason for a failed attempt
type AttemptFailureReason string

const (
	AttemptFailureReasonError    AttemptFailureReason = "error"
	AttemptFailureReasonTimeout  AttemptFailureReason = "timeout"
	AttemptFailureReasonCanceled AttemptFailureReason = "canceled"
	AttemptFailureReasonResult   AttemptFailureReason = "result"
)

// Attempt contains information about a single retry attempt
type Attempt struct {
	PolicyName string
	Number     int
	Timestamp  time.Time
	Duration   time.Duration

	Status        AttemptStatus
	FailureReason AttemptFailureReason
	Error         error
	Retryable     bool
}

func (a Attempt) IsSuccess() bool {
	return a.Status == AttemptStatusSuccess
}

// Outcome contains information about the complete retry sequence
type Outcome struct {
	PolicyName    string
	TotalAttempts int
	TotalDuration time.Duration

	Status        OutcomeStatus
	FailureReason OutcomeFailureReason
}

func (o Outcome) IsSuccess() bool {
	return o.Status == OutcomeStatusSuccess
}

// Metrics defines the interface for retry instrumentation
type Metrics interface {
	// RecordAttempt records metrics for a single attempt
	RecordAttempt(ctx context.Context, result Attempt)

	// RecordOutcome records the final outcome of a retry sequence
	RecordOutcome(ctx context.Context, outcome Outcome)

	// RecordBackoff records time spent waiting between attempts
	RecordBackoff(ctx context.Context, policyName string, attempt int, duration time.Duration)
}

// NoopMetrics is a no-operation implementation of the Metrics interface
type NoopMetrics struct{}

// RecordAttempt is a no-op implementation
func (n *NoopMetrics) RecordAttempt(ctx context.Context, result Attempt) {
	// No operation
}

// RecordOutcome is a no-op implementation
func (n *NoopMetrics) RecordOutcome(ctx context.Context, outcome Outcome) {
	// No operation
}

// RecordBackoff is a no-op implementation
func (n *NoopMetrics) RecordBackoff(ctx context.Context, policyName string, attempt int, duration time.Duration) {
	// No operation
}

// SetGlobalMetrics sets the global Metrics implementation
func SetGlobalMetrics(m Metrics) {
	if m == nil {
		m = &NoopMetrics{}
	}

	_globalMetrics.Store(m)
}

func GetGlobalMetrics() Metrics {
	m := _globalMetrics.Load()
	if m == nil {
		return &NoopMetrics{}
	}
	return m.(Metrics)
}
