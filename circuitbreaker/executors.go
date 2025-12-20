package circuitbreaker

import (
	"context"
	"errors"
	"runtime/debug"
	"time"
)

type PanicError struct {
	Recover any
	Cause   error
	Stack   []byte
}

func (r *PanicError) Error() string {
	return "circuitbreaker: panic occurred"
}

func (r *PanicError) Unwrap() error {
	return r.Cause
}

func IsPanicError(err error) bool {
	var panicError *PanicError
	ok := errors.As(err, &panicError)
	return ok
}

func safeExecute[T any](ctx context.Context, fn func(ctx context.Context) (T, error)) (result T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{
				Recover: r,
				Cause:   err,
				Stack:   debug.Stack(),
			}
		}
	}()

	if ctx.Err() != nil {
		return result, ctx.Err()
	}

	return fn(ctx)
}

func Execute[T any](ctx context.Context, cb CircuitBreaker, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := cb.before(); err != nil {
		return zero, err
	}

	start := time.Now()

	result, err := safeExecute(ctx, fn)
	cb.after(result, err, time.Since(start))
	return result, err
}

func Do(ctx context.Context, cb CircuitBreaker, fn func(context.Context) error) (err error) {
	_, err = Execute(ctx, cb, func(ctx context.Context) (any, error) {
		return nil, fn(ctx)
	})

	return err
}
