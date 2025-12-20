package retry

import (
	"errors"
	"time"

	"github.com/hugolhafner/dskit/backoff"
	"github.com/hugolhafner/dskit/circuitbreaker"
)

type Policy struct {
	// name is the name of the policy
	name string

	// metrics is the metrics reporter for the policy
	// if nil, uses the global metrics instance
	metrics Metrics

	// maxAttempts is the maximum number of attempts
	// including the initial call as the first attempt
	maxAttempts int

	// attemptTimeout is the maximum duration for each attempt
	// If zero, attempts have no timeout
	attemptTimeout time.Duration

	// backoff is the function to calculate the wait duration between attempts
	backoff backoff.Backoff

	// retryOnResultPredicate is the predicate to determine if a result should trigger a retry
	// true means retry, false means do not retry
	retryOnResultPredicate func(any) bool

	// retryOnErrorPredicate is the predicate to determine if an error should trigger a retry
	// true means retry, false means do not retry
	retryOnErrorPredicate func(error) bool

	// retryErrors is a list of error types that should trigger a retry
	retryErrors []error

	// ignoreErrors is a list of error types that should not trigger a retry
	ignoreErrors []error
}

type Option func(*Policy)

func WithMetrics(metrics Metrics) Option {
	return func(p *Policy) {
		p.metrics = metrics
	}
}

func WithMaxAttempts(attempts int) Option {
	return func(p *Policy) {
		p.maxAttempts = attempts
	}
}

func WithAttemptTimeout(timeout time.Duration) Option {
	return func(p *Policy) {
		p.attemptTimeout = timeout
	}
}

func WithBackoff(f backoff.Backoff) Option {
	return func(p *Policy) {
		p.backoff = f
	}
}

// WithRetryOnResultPredicate sets a custom predicate function to determine
// whether a result should trigger a retry.
func WithRetryOnResultPredicate(predicate func(any) bool) Option {
	return func(p *Policy) {
		p.retryOnResultPredicate = predicate
	}
}

// WithRetryOnErrorPredicate sets a custom predicate function to determine
// whether an error should trigger a retry. If this exists, it takes precedence
// over the retryErrors and ignoreErrors lists.
func WithRetryOnErrorPredicate(predicate func(error) bool) Option {
	return func(p *Policy) {
		p.retryOnErrorPredicate = predicate
	}
}

func WithRetryErrors(errors ...error) Option {
	return func(p *Policy) {
		p.retryErrors = errors
	}
}

func WithIgnoreErrors(errors ...error) Option {
	return func(p *Policy) {
		p.ignoreErrors = errors
	}
}

func (p *Policy) Validate() error {
	if p.maxAttempts < 1 {
		return &ValidationError{Field: "maxAttempts", Message: "must be at least 1"}
	}

	if p.backoff == nil {
		return &ValidationError{
			Field:   "backoff",
			Message: "backoff must be set",
		}
	}

	return nil
}

func NewPolicy(name string, options ...Option) (*Policy, error) {
	b := backoff.NewLinear(100 * time.Millisecond)

	policy := &Policy{
		name:        name,
		maxAttempts: 3,
		backoff:     b,
	}

	for _, option := range options {
		option(policy)
	}

	if err := policy.Validate(); err != nil {
		return nil, err
	}

	return policy, nil
}

func NewCircuitAwarePolicy(name string, opts ...Option) (*Policy, error) {
	baseOpts := []Option{
		WithIgnoreErrors(
			circuitbreaker.ErrOpenState,
			circuitbreaker.ErrHalfOpenState,
		),
	}

	return NewPolicy(name, append(baseOpts, opts...)...)
}

func MustNewPolicy(name string, options ...Option) *Policy {
	policy, err := NewPolicy(name, options...)
	if err != nil {
		panic(err)
	}

	return policy
}

func MustNewCircuitAwarePolicy(name string, opts ...Option) *Policy {
	policy, err := NewCircuitAwarePolicy(name, opts...)
	if err != nil {
		panic(err)
	}

	return policy
}

func (p *Policy) metricsReporter() Metrics {
	if p.metrics != nil {
		return p.metrics
	}

	return GetGlobalMetrics()
}

func (p *Policy) ShouldRetryError(err error) bool {
	if err == nil {
		return false
	}

	if p.retryOnErrorPredicate != nil {
		return p.retryOnErrorPredicate(err)
	}

	for _, ignoreErr := range p.ignoreErrors {
		if errors.Is(err, ignoreErr) {
			return false
		}
	}

	// If allowlist is defined, error must match
	if len(p.retryErrors) > 0 {
		for _, retryErr := range p.retryErrors {
			if errors.Is(err, retryErr) {
				return true
			}
		}

		return false
	}

	return true
}

func (p *Policy) Name() string {
	return p.name
}

func (p *Policy) Clone(name string) *Policy {
	clone := &Policy{
		name:                   name,
		metrics:                p.metrics,
		maxAttempts:            p.maxAttempts,
		attemptTimeout:         p.attemptTimeout,
		backoff:                p.backoff,
		retryOnResultPredicate: p.retryOnResultPredicate,
		retryOnErrorPredicate:  p.retryOnErrorPredicate,
		retryErrors:            nil,
		ignoreErrors:           nil,
	}

	if len(p.retryErrors) > 0 {
		clone.retryErrors = make([]error, len(p.retryErrors))
		copy(clone.retryErrors, p.retryErrors)
	}

	if len(p.ignoreErrors) > 0 {
		clone.ignoreErrors = make([]error, len(p.ignoreErrors))
		copy(clone.ignoreErrors, p.ignoreErrors)
	}

	return clone
}

func (p *Policy) MaxAttempts() int {
	return p.maxAttempts
}

func (p *Policy) AttemptTimeout() time.Duration {
	return p.attemptTimeout
}

func (p *Policy) Backoff() backoff.Backoff {
	return p.backoff
}

func (p *Policy) RetryOnResultPredicate() func(any) bool {
	return p.retryOnResultPredicate
}

func (p *Policy) RetryOnErrorPredicate() func(error) bool {
	return p.retryOnErrorPredicate
}

func (p *Policy) RetryErrors() []error {
	if len(p.retryErrors) == 0 {
		return nil
	}

	var errs = make([]error, len(p.retryErrors))
	copy(errs, p.retryErrors)
	return errs
}

func (p *Policy) IgnoreErrors() []error {
	if len(p.ignoreErrors) == 0 {
		return nil
	}

	var errs = make([]error, len(p.ignoreErrors))
	copy(errs, p.ignoreErrors)
	return errs
}
