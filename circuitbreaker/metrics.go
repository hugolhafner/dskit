package circuitbreaker

import (
	"context"
	"sync/atomic"
	"time"
)

var _ Metrics = (*NoopMetrics)(nil)

var _globalMetrics = atomic.Value{}

// StateTransition represents a circuit breaker state change
type StateTransition struct {
	Name      string
	FromState State
	ToState   State
	Timestamp time.Time
}

// CallResult represents the result of a call through the circuit breaker
type CallResult struct {
	Name     string
	Outcome  CallOutcome
	Duration time.Duration
	Error    error
}

// CallRejection represents a call that was rejected by the circuit breaker
type CallRejection struct {
	Name  string
	State State
	Error error
}

// CallRates represents the current call rate statistics
type CallRates struct {
	Name         string
	SuccessRate  float64
	FailureRate  float64
	SlowCallRate float64
	TotalCalls   int
}

// Metrics defines the interface for circuit breaker instrumentation
type Metrics interface {
	// RecordStateTransition records a state transition event
	RecordStateTransition(ctx context.Context, transition StateTransition)

	// RecordCallResult records the result of a call that was permitted
	RecordCallResult(ctx context.Context, result CallResult)

	// RecordCallRejection records a call that was rejected due to circuit breaker state
	RecordCallRejection(ctx context.Context, rejection CallRejection)

	// RecordCallRates records the current call rate statistics
	RecordCallRates(ctx context.Context, rates CallRates)
}

// NoopMetrics is a no-operation implementation of the Metrics interface
type NoopMetrics struct{}

func (n *NoopMetrics) RecordStateTransition(ctx context.Context, transition StateTransition) {}

func (n *NoopMetrics) RecordCallResult(ctx context.Context, result CallResult) {}

func (n *NoopMetrics) RecordCallRejection(ctx context.Context, rejection CallRejection) {}

func (n *NoopMetrics) RecordCallRates(ctx context.Context, rates CallRates) {}

// SetGlobalMetrics sets the global Metrics implementation
func SetGlobalMetrics(m Metrics) {
	if m == nil {
		m = &NoopMetrics{}
	}
	_globalMetrics.Store(m)
}

// GetGlobalMetrics returns the global Metrics implementation
func GetGlobalMetrics() Metrics {
	m := _globalMetrics.Load()
	if m == nil {
		return &NoopMetrics{}
	}
	return m.(Metrics)
}
