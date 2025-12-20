package backoff

import (
	"math"
	"math/rand/v2"
	"time"
)

var _ Backoff = (*Exponential)(nil)

type Exponential struct {
	initialInterval time.Duration
	maxInterval     time.Duration
	multiplier      float64
	jitter          float64
}

type ExponentialOption func(*Exponential)

func WithInitialInterval(d time.Duration) ExponentialOption {
	return func(e *Exponential) {
		e.initialInterval = d
	}
}

func WithMaxInterval(d time.Duration) ExponentialOption {
	return func(e *Exponential) {
		e.maxInterval = d
	}
}

func WithMultiplier(m float64) ExponentialOption {
	return func(e *Exponential) {
		e.multiplier = m
	}
}

func WithJitter(j float64) ExponentialOption {
	return func(e *Exponential) {
		e.jitter = j
	}
}

func NewExponential(opts ...ExponentialOption) Exponential {
	e := Exponential{
		initialInterval: 500 * time.Millisecond,
		maxInterval:     10 * time.Second,
		multiplier:      2.0,
		jitter:          0.0,
	}

	for _, opt := range opts {
		opt(&e)
	}

	return e
}

func (e Exponential) Next(attempt uint) time.Duration {
	interval := float64(e.initialInterval) * math.Pow(e.multiplier, float64(attempt-1))

	if e.jitter > 0 {
		jitter := interval * e.jitter * (2*rand.Float64() - 1)
		interval = max(0, interval+jitter)
	}

	if interval > float64(e.maxInterval) {
		interval = float64(e.maxInterval)
	}

	return time.Duration(interval)
}
