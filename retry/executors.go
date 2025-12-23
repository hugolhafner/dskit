package retry

import (
	"context"
	"errors"
	"time"

	"github.com/hugolhafner/dskit/circuitbreaker"
)

type waiter func(time.Duration) error

func contextWaiter(ctx context.Context) waiter {
	return func(d time.Duration) error {
		timer := time.NewTimer(d)
		defer func() {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}

func safeExecute[T any](ctx context.Context, fn func(ctx context.Context) (T, error)) (result T, err error) {
	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	return fn(ctx)
}

func classifyAttemptFailure(err error) AttemptFailureReason {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return AttemptFailureReasonTimeout
	}

	if errors.Is(err, context.Canceled) {
		return AttemptFailureReasonCanceled
	}

	return AttemptFailureReasonError
}

func classifyContextError(err error) OutcomeFailureReason {
	if err == nil {
		return ""
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeFailureReasonTimeout
	}

	return OutcomeFailureReasonCanceled
}

type attemptOutcome[T any] struct {
	result    T
	attempt   Attempt
	success   bool
	retryable bool
}

func executeAttempt[T any](
	ctx context.Context,
	p *Policy,
	attemptNum int,
	fn func(ctx context.Context) (T, error),
) attemptOutcome[T] {
	attemptStart := time.Now()

	attempt := Attempt{
		PolicyName: p.name,
		Number:     attemptNum,
		Timestamp:  attemptStart,
	}

	var (
		attemptCtx    context.Context
		attemptCancel context.CancelFunc
	)
	if p.attemptTimeout > 0 {
		attemptCtx, attemptCancel = context.WithTimeout(ctx, p.attemptTimeout)
	} else {
		attemptCtx, attemptCancel = context.WithCancel(ctx)
	}
	defer attemptCancel()

	attemptResult, attemptErr := safeExecute(attemptCtx, fn)
	attempt.Duration = time.Since(attemptStart)

	shouldRetryResult := attemptErr == nil &&
		p.retryOnResultPredicate != nil &&
		p.retryOnResultPredicate(attemptResult)

	if attemptErr == nil && !shouldRetryResult {
		attempt.Status = AttemptStatusSuccess
		return attemptOutcome[T]{
			result:  attemptResult,
			attempt: attempt,
			success: true,
		}
	}

	attempt.Status = AttemptStatusError

	if shouldRetryResult {
		attempt.Error = ErrResultPredicateRetry
		attempt.FailureReason = AttemptFailureReasonResult
		attempt.Retryable = true
	} else {
		attempt.Error = attemptErr
		attempt.FailureReason = classifyAttemptFailure(attemptErr)
		attempt.Retryable = p.ShouldRetryError(attemptErr)
	}

	var zero T
	return attemptOutcome[T]{
		result:    zero,
		attempt:   attempt,
		retryable: attempt.Retryable,
	}
}

func execute[T any](ctx context.Context, p *Policy, wait waiter, fn func(ctx context.Context) (T, error)) (T, error) {
	var (
		result          T
		attemptCount    = 1
		metricsReporter = p.metricsReporter()
		overallStart    = time.Now()
	)

	retryErr := &RetryError{
		Attempts: make([]Attempt, 0, p.maxAttempts),
	}

	outcome := Outcome{
		PolicyName: p.name,
		Status:     OutcomeStatusError,
	}

	defer func() {
		outcome.TotalAttempts = attemptCount
		outcome.TotalDuration = time.Since(overallStart)
		metricsReporter.RecordOutcome(ctx, outcome)
	}()

	for {
		ao := executeAttempt(ctx, p, attemptCount, fn)
		metricsReporter.RecordAttempt(ctx, ao.attempt)

		if ao.success {
			result = ao.result
			outcome.Status = OutcomeStatusSuccess
			return result, nil
		}

		retryErr.Attempts = append(retryErr.Attempts, ao.attempt)

		if !ao.retryable {
			outcome.FailureReason = OutcomeFailureReasonNonRetryable
			break
		}

		if attemptCount >= p.maxAttempts {
			outcome.FailureReason = OutcomeFailureReasonExhausted
			break
		}

		backoffDuration := p.backoff.Next(uint(attemptCount))
		if waitErr := wait(backoffDuration); waitErr != nil {
			outcome.FailureReason = classifyContextError(waitErr)
			retryErr.TerminationError = waitErr
			return result, retryErr
		}

		attemptCount++
		metricsReporter.RecordBackoff(ctx, p.name, attemptCount, backoffDuration)
	}

	return result, retryErr
}

func Do(ctx context.Context, p *Policy, fn func(context.Context) error) error {
	_, err := Execute(ctx, p, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

func DoWithCircuit(ctx context.Context, p *Policy, cb circuitbreaker.CircuitBreaker, fn func(context.Context) error) error {
	_, err := ExecuteWithCircuit(ctx, p, cb, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, fn(ctx)
	})
	return err
}

func Execute[T any](ctx context.Context, p *Policy, fn func(context.Context) (T, error)) (T, error) {
	return execute(ctx, p, contextWaiter(ctx), fn)
}

func ExecuteWithCircuit[T any](ctx context.Context, p *Policy, cb circuitbreaker.CircuitBreaker, fn func(context.Context) (T, error)) (T, error) {
	return execute(ctx, p, contextWaiter(ctx), func(ctx context.Context) (T, error) {
		return circuitbreaker.Execute[T](ctx, cb, fn)
	})
}
