package circuitbreaker

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics:
// circuitbreaker_calls_total (Counter) - Total number of calls through the circuit breaker
// * name (string) - The name of the circuit breaker
// * outcome (string) - The outcome of the call ("success", "failure", "slow_success", "slow_failure")
//
// circuitbreaker_calls_duration_milliseconds (Histogram) - Duration of calls in milliseconds
// * name (string) - The name of the circuit breaker
// * outcome (string) - The outcome of the call
//
// circuitbreaker_rejections_total (Counter) - Total number of rejected calls
// * name (string) - The name of the circuit breaker
// * state (string) - The state that caused rejection ("open", "half_open")
//
// circuitbreaker_state_transitions_total (Counter) - Total number of state transitions
// * name (string) - The name of the circuit breaker
// * from_state (string) - The previous state
// * to_state (string) - The new state
//
// circuitbreaker_state (Gauge) - Current state of the circuit breaker (0=closed, 1=half_open, 2=open, 3=metrics_only)
// * name (string) - The name of the circuit breaker
//
// circuitbreaker_failure_rate (Gauge) - Current failure rate percentage
// * name (string) - The name of the circuit breaker
//
// circuitbreaker_slow_call_rate (Gauge) - Current slow call rate percentage
// * name (string) - The name of the circuit breaker

const (
	instrumentationName    = "github.com/hugolhafner/dskit/circuitbreaker"
	instrumentationVersion = "v0.1.0" // x-release-please
)

const (
	unitCall         = "{call}"
	unitRejection    = "{rejection}"
	unitTransition   = "{transition}"
	unitMilliseconds = "ms"
	unitPercent      = "%"
)

var _ Metrics = (*OTelMetrics)(nil)

type OTelMetrics struct {
	callsTotal    metric.Int64Counter
	callsDuration metric.Float64Histogram

	rejectionsTotal metric.Int64Counter

	stateTransitionsTotal metric.Int64Counter
	currentState          metric.Int64Gauge

	failureRate  metric.Float64Gauge
	slowCallRate metric.Float64Gauge
}

type OTelConfig struct {
	MeterProvider metric.MeterProvider
	MetricPrefix  string
	Attributes    []attribute.KeyValue
}

type OTelOption func(*OTelConfig)

func WithMeterProvider(meterProvider metric.MeterProvider) OTelOption {
	return func(cfg *OTelConfig) {
		cfg.MeterProvider = meterProvider
	}
}

func WithMetricPrefix(prefix string) OTelOption {
	return func(cfg *OTelConfig) {
		cfg.MetricPrefix = prefix
	}
}

func WithAttributes(attrs []attribute.KeyValue) OTelOption {
	return func(cfg *OTelConfig) {
		copied := make([]attribute.KeyValue, len(attrs))
		copy(copied, attrs)
		cfg.Attributes = copied
	}
}

func NewOTelMetrics(opts ...OTelOption) (*OTelMetrics, error) {
	cfg := &OTelConfig{
		MeterProvider: otel.GetMeterProvider(),
		MetricPrefix:  "circuitbreaker_",
		Attributes:    []attribute.KeyValue{},
	}

	for _, opt := range opts {
		opt(cfg)
	}

	meter := cfg.MeterProvider.Meter(instrumentationName, metric.WithInstrumentationVersion(instrumentationVersion))

	callsTotal, err := meter.Int64Counter(
		cfg.MetricPrefix+"calls_total",
		metric.WithDescription("Total number of calls through the circuit breaker"),
		metric.WithUnit(unitCall),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create calls_total counter: %w", err)
	}

	callsDuration, err := meter.Float64Histogram(
		cfg.MetricPrefix+"calls_duration_milliseconds",
		metric.WithDescription("Duration of calls in milliseconds"),
		metric.WithUnit(unitMilliseconds),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create calls_duration_milliseconds histogram: %w", err)
	}

	rejectionsTotal, err := meter.Int64Counter(
		cfg.MetricPrefix+"rejections_total",
		metric.WithDescription("Total number of rejected calls"),
		metric.WithUnit(unitRejection),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create rejections_total counter: %w", err)
	}

	stateTransitionsTotal, err := meter.Int64Counter(
		cfg.MetricPrefix+"state_transitions_total",
		metric.WithDescription("Total number of state transitions"),
		metric.WithUnit(unitTransition),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create state_transitions_total counter: %w", err)
	}

	currentState, err := meter.Int64Gauge(
		cfg.MetricPrefix+"state",
		metric.WithDescription("Current state of the circuit breaker"),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create state gauge: %w", err)
	}

	failureRate, err := meter.Float64Gauge(
		cfg.MetricPrefix+"failure_rate",
		metric.WithDescription("Current failure rate percentage"),
		metric.WithUnit(unitPercent),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create failure_rate gauge: %w", err)
	}

	slowCallRate, err := meter.Float64Gauge(
		cfg.MetricPrefix+"slow_call_rate",
		metric.WithDescription("Current slow call rate percentage"),
		metric.WithUnit(unitPercent),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create slow_call_rate gauge: %w", err)
	}

	return &OTelMetrics{
		callsTotal:            callsTotal,
		callsDuration:         callsDuration,
		rejectionsTotal:       rejectionsTotal,
		stateTransitionsTotal: stateTransitionsTotal,
		currentState:          currentState,
		failureRate:           failureRate,
		slowCallRate:          slowCallRate,
	}, nil
}

func outcomeString(outcome CallOutcome) string {
	switch outcome {
	case OutcomeSuccess:
		return "success"
	case OutcomeFailure:
		return "failure"
	case OutcomeSlowSuccess:
		return "slow_success"
	case OutcomeSlowFailure:
		return "slow_failure"
	default:
		return "unknown"
	}
}

func stateString(state State) string {
	switch state {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half_open"
	case StateOpen:
		return "open"
	case StateMetricsOnly:
		return "metrics_only"
	default:
		return "unknown"
	}
}

func (m *OTelMetrics) RecordStateTransition(ctx context.Context, transition StateTransition) {
	attrs := []attribute.KeyValue{
		attribute.String("name", transition.Name),
		attribute.String("from_state", stateString(transition.FromState)),
		attribute.String("to_state", stateString(transition.ToState)),
	}

	m.stateTransitionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))

	for state := StateClosed; state <= StateMetricsOnly; state++ {
		var value int64
		if state == transition.ToState {
			value = 1
		} else {
			value = 0
		}

		m.currentState.Record(
			ctx, value, metric.WithAttributes(
				attribute.String("name", transition.Name), attribute.String("state", stateString(state)),
			),
		)
	}
}

func (m *OTelMetrics) RecordCallResult(ctx context.Context, result CallResult) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("name", result.Name),
		attribute.String("outcome", outcomeString(result.Outcome)),
	}

	m.callsTotal.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	m.callsDuration.Record(ctx, float64(result.Duration.Milliseconds()), metric.WithAttributes(baseAttrs...))
}

func (m *OTelMetrics) RecordCallRejection(ctx context.Context, rejection CallRejection) {
	attrs := []attribute.KeyValue{
		attribute.String("name", rejection.Name),
		attribute.String("state", stateString(rejection.State)),
	}

	m.rejectionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (m *OTelMetrics) RecordCallRates(ctx context.Context, rates CallRates) {
	nameAttr := attribute.String("name", rates.Name)

	m.failureRate.Record(ctx, rates.FailureRate, metric.WithAttributes(nameAttr))
	m.slowCallRate.Record(ctx, rates.SlowCallRate, metric.WithAttributes(nameAttr))
}
