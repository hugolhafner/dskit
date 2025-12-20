package retry

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics:
// retry_attempts_total (Counter) - Total number of retry attempts made
// * policy (string) - The name of the retry policy
//
// retry_attempts_success_total (Counter) - Total number of successful retry attempts
// * policy (string) - The name of the retry policy
//
// retry_attempts_failure_total (Counter) - Total number of failed retry attempts
// * policy (string) - The name of the retry policy
// * reason (string) - The reason for failure ("error", "timeout", "canceled", "status")
// * retryable (bool) - Whether the failure was considered retryable
//
// retry_attempts_duration_milliseconds (Histogram) - Duration of retry attempts in milliseconds
// * policy (string) - The name of the retry policy
// * outcome (string) - The outcome of the attempt ("success", "failure")
//
// retry_attempts_buckets (Histogram) - Buckets for retry attempt counts
// * policy (string) - The name of the retry policy
//
// retry_outcome_total (Counter) - Total number of retry outcomes
//
// retry_outcome_success_total (Counter) - Total number of successful retry outcomes
// * policy (string) - The name of the retry policy
//
// retry_outcome_failure_total (Counter) - Total number of failed retry outcomes
// * policy (string) - The name of the retry policy
// * reason (string) - The reason for failure ("exhausted", "canceled", "non_retryable")
//
// retry_outcome_duration_milliseconds (Histogram) - Duration of retry outcome in milliseconds
// * policy (string) - The name of the retry policy
//
// retry_backoff_duration_milliseconds (Histogram) - Duration of backoff periods in milliseconds
// * policy (string) - The name of the retry policy
//

const (
	instrumentationName    = "github.com/hugolhafner/dskit/retry"
	instrumentationVersion = "v0.1.0" // x-release-please
)

const (
	unitAttempt      = "{attempt}"
	unitOutcome      = "{outcome}"
	unitMilliseconds = "ms"
)

var _ Metrics = (*OTelMetrics)(nil)

type OTelMetrics struct {
	attemptsTotal    metric.Int64Counter
	attemptsSuccess  metric.Int64Counter
	attemptsFailure  metric.Int64Counter
	attemptsDuration metric.Float64Histogram

	outcomeTotal          metric.Int64Counter
	outcomeSuccess        metric.Int64Counter
	outcomeFailure        metric.Int64Counter
	outcomeDuration       metric.Float64Histogram
	outcomeAttemptsBucket metric.Int64Histogram

	backoffDuration metric.Float64Histogram
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
		MetricPrefix:  "retry_",
		Attributes:    []attribute.KeyValue{},
	}

	for _, opt := range opts {
		opt(cfg)
	}

	meter := cfg.MeterProvider.Meter(instrumentationName, metric.WithInstrumentationVersion(instrumentationVersion))

	attemptsTotal, err := meter.Int64Counter(
		cfg.MetricPrefix+"attempts_total",
		metric.WithDescription("Total number of retry attempts made"),
		metric.WithUnit(unitAttempt),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create attempts_total counter: %w", err)
	}

	attemptsSuccess, err := meter.Int64Counter(
		cfg.MetricPrefix+"attempts_success_total",
		metric.WithDescription("Total number of successful retry attempts"),
		metric.WithUnit(unitAttempt),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create attempts_success_total counter: %w", err)
	}

	attemptsFailure, err := meter.Int64Counter(
		cfg.MetricPrefix+"attempts_failure_total",
		metric.WithDescription("Total number of failed retry attempts"),
		metric.WithUnit(unitAttempt),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create attempts_failure_total counter: %w", err)
	}

	attemptsDuration, err := meter.Float64Histogram(
		cfg.MetricPrefix+"attempts_duration_milliseconds",
		metric.WithDescription("Duration of retry attempts in milliseconds"),
		metric.WithUnit(unitMilliseconds),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create attempts_duration_milliseconds histogram: %w", err)
	}

	outcomeAttemptsBucket, err := meter.Int64Histogram(
		cfg.MetricPrefix+"attempts_buckets",
		metric.WithDescription("Buckets for retry attempt counts"),
		metric.WithUnit(unitAttempt),
		metric.WithExplicitBucketBoundaries(1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 15, 20, 30, 50, 100),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create attempts_buckets histogram: %w", err)
	}

	outcomeTotal, err := meter.Int64Counter(
		cfg.MetricPrefix+"outcome_total",
		metric.WithDescription("Total number of retry outcomes"),
		metric.WithUnit(unitOutcome),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create outcome_total counter: %w", err)
	}

	outcomeSuccess, err := meter.Int64Counter(
		cfg.MetricPrefix+"outcome_success_total",
		metric.WithDescription("Total number of successful retry outcomes"),
		metric.WithUnit(unitOutcome),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create outcome_success_total counter: %w", err)
	}

	outcomeFailure, err := meter.Int64Counter(
		cfg.MetricPrefix+"outcome_failure_total",
		metric.WithDescription("Total number of failed retry outcomes"),
		metric.WithUnit(unitOutcome),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create outcome_failure_total counter: %w", err)
	}

	outcomeDuration, err := meter.Float64Histogram(
		cfg.MetricPrefix+"outcome_duration_milliseconds",
		metric.WithDescription("Duration of retry outcome in milliseconds"),
		metric.WithUnit(unitMilliseconds),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create outcome_duration_milliseconds histogram: %w", err)
	}

	backoffDuration, err := meter.Float64Histogram(
		cfg.MetricPrefix+"backoff_duration_milliseconds",
		metric.WithDescription("Duration of backoff periods in milliseconds"),
		metric.WithUnit(unitMilliseconds),
		metric.WithExplicitBucketBoundaries(0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create backoff_duration_milliseconds histogram: %w", err)
	}

	return &OTelMetrics{
		attemptsTotal:         attemptsTotal,
		attemptsSuccess:       attemptsSuccess,
		attemptsFailure:       attemptsFailure,
		attemptsDuration:      attemptsDuration,
		outcomeTotal:          outcomeTotal,
		outcomeSuccess:        outcomeSuccess,
		outcomeFailure:        outcomeFailure,
		outcomeDuration:       outcomeDuration,
		outcomeAttemptsBucket: outcomeAttemptsBucket,
		backoffDuration:       backoffDuration,
	}, nil
}

func (m *OTelMetrics) RecordAttempt(ctx context.Context, attempt Attempt) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("policy", attempt.PolicyName),
	}

	m.attemptsTotal.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	m.attemptsDuration.Record(ctx, float64(attempt.Duration.Milliseconds()),
		metric.WithAttributes(append(baseAttrs, attribute.String("status", string(attempt.Status)))...))

	if attempt.IsSuccess() {
		m.attemptsSuccess.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	} else {
		m.attemptsFailure.Add(ctx, 1, metric.WithAttributes(append(baseAttrs,
			attribute.String("reason", string(attempt.FailureReason)),
			attribute.Bool("retryable", attempt.Retryable),
		)...))
	}
}

func (m *OTelMetrics) RecordOutcome(ctx context.Context, outcome Outcome) {
	baseAttrs := []attribute.KeyValue{
		attribute.String("policy", outcome.PolicyName),
	}

	m.outcomeTotal.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	m.outcomeAttemptsBucket.Record(ctx, int64(outcome.TotalAttempts), metric.WithAttributes(baseAttrs...))
	m.outcomeDuration.Record(ctx, float64(outcome.TotalDuration.Milliseconds()),
		metric.WithAttributes(append(baseAttrs, attribute.String("status", string(outcome.Status)))...))

	if outcome.IsSuccess() {
		m.outcomeSuccess.Add(ctx, 1, metric.WithAttributes(baseAttrs...))
	} else {
		m.outcomeFailure.Add(ctx, 1, metric.WithAttributes(append(baseAttrs,
			attribute.String("reason", string(outcome.FailureReason)),
		)...))
	}
}

func (m *OTelMetrics) RecordBackoff(ctx context.Context, policyName string, attempt int, duration time.Duration) {
	m.backoffDuration.Record(ctx, float64(duration.Milliseconds()), metric.WithAttributes(
		attribute.String("policy", policyName),
	))
}
