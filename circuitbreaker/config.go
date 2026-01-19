package circuitbreaker

import (
	"time"
)

type Config struct {
	Window Window

	Metrics Metrics

	// MetricsOnlyMode starts the circuit breaker in metrics only mode,
	// where it does not block any calls but still collects metrics
	MetricsOnlyMode bool

	// MinimumNumberOfCalls is the minimum number of calls required before
	// the circuit breaker evaluates the failure rate and slow call rate
	MinimumNumberOfCalls int

	// FailureRateThreshold is the failure rate threshold in percentage to trip the circuit breaker
	FailureRateThreshold float64

	// SlowCallRateThreshold is the slow call rate threshold in percentage to trip the circuit breaker
	SlowCallRateThreshold float64

	// SlowCallDurationThreshold is the duration above which a call is considered slow
	SlowCallDurationThreshold time.Duration

	// PermittedNumberOfCallsInHalfOpenState is the number of permitted calls when the circuit breaker is half-open
	// before evaluating the thresholds again
	PermittedNumberOfCallsInHalfOpenState int

	// WaitDurationInOpenState is the duration the circuit breaker stays open before transitioning to half-open
	WaitDurationInOpenState time.Duration

	FailOnResultPredicate func(result any) bool
	FailOnErrorPredicate  func(error) bool

	FailErrors   []error
	IgnoreErrors []error
}

type Option func(*Config)

func defaultConfig() Config {
	return Config{
		Window:                                NewCountWindow(100),
		MetricsOnlyMode:                       false,
		MinimumNumberOfCalls:                  20,
		FailureRateThreshold:                  50.0,
		SlowCallRateThreshold:                 50.0,
		SlowCallDurationThreshold:             10 * time.Second,
		PermittedNumberOfCallsInHalfOpenState: 10,
		WaitDurationInOpenState:               60 * time.Second,
	}
}

func WithMetricsOnlyMode() Option {
	return func(c *Config) {
		c.MetricsOnlyMode = true
	}
}

func WithMetrics(metrics Metrics) Option {
	return func(c *Config) {
		c.Metrics = metrics
	}
}

func WithWindow(window Window) Option {
	return func(c *Config) {
		c.Window = window
	}
}

func WithMinimumNumberOfCalls(n int) Option {
	return func(c *Config) {
		c.MinimumNumberOfCalls = n
	}
}

func WithFailureRateThreshold(threshold float64) Option {
	return func(c *Config) {
		c.FailureRateThreshold = threshold
	}
}

func WithSlowCallRateThreshold(threshold float64) Option {
	return func(c *Config) {
		c.SlowCallRateThreshold = threshold
	}
}

func WithSlowCallDurationThreshold(duration time.Duration) Option {
	return func(c *Config) {
		c.SlowCallDurationThreshold = duration
	}
}

func WithPermittedNumberOfCallsInHalfOpenState(n int) Option {
	return func(c *Config) {
		c.PermittedNumberOfCallsInHalfOpenState = n
	}
}

func WithWaitDurationInOpenState(duration time.Duration) Option {
	return func(c *Config) {
		c.WaitDurationInOpenState = duration
	}
}

func WithFailOnResultPredicate(predicate func(result any) bool) Option {
	return func(c *Config) {
		c.FailOnResultPredicate = predicate
	}
}

func WithFailOnErrorPredicate(predicate func(error) bool) Option {
	return func(c *Config) {
		c.FailOnErrorPredicate = predicate
	}
}
func WithFailErrors(errors ...error) Option {
	return func(c *Config) {
		c.FailErrors = errors
	}
}

func WithIgnoreErrors(errors ...error) Option {
	return func(c *Config) {
		c.IgnoreErrors = errors
	}
}
