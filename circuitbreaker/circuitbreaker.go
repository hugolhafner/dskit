package circuitbreaker

import (
	"errors"
	"sync"
	"time"
)

type State int

const (
	StateClosed State = iota
	StateHalfOpen
	StateOpen
	StateMetricsOnly
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "CLOSED"
	case StateHalfOpen:
		return "HALF_OPEN"
	case StateOpen:
		return "OPEN"
	case StateMetricsOnly:
		return "METRICS_ONLY"
	default:
		return "UNKNOWN"
	}
}

var (
	ErrOpenState     = errors.New("circuitbreaker: open state")
	ErrHalfOpenState = errors.New("circuitbreaker: half-open state with no available calls")
)

func IsCallNotPermittedError(err error) bool {
	return errors.Is(err, ErrOpenState) || errors.Is(err, ErrHalfOpenState)
}

type CircuitBreaker interface {
	Name() string
	State() State

	before() error
	after(result any, err error, duration time.Duration)
}

var _ CircuitBreaker = (*circuitBreakerImpl)(nil)

type circuitBreakerImpl struct {
	name   string
	window Window
	config Config

	// metrics Metrics

	mu             sync.RWMutex
	state          State
	transitionTime time.Time

	halfOpenCompletedLeases int
	halfOpenLeases          int
}

func New(name string, opts ...Option) CircuitBreaker {
	config := defaultConfig()
	for _, opt := range opts {
		opt(&config)
	}

	// TODO: Metrics

	return &circuitBreakerImpl{
		name:   name,
		config: config,
		state:  StateClosed,
		window: config.Window,
	}
}

func (cb *circuitBreakerImpl) Name() string {
	return cb.name
}

func (cb *circuitBreakerImpl) State() State {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

func (cb *circuitBreakerImpl) setStateUnsafe(state State) {
	if cb.state == state {
		return
	}

	if state == StateHalfOpen {
		cb.halfOpenLeases = cb.config.PermittedNumberOfCallsInHalfOpenState
		cb.halfOpenCompletedLeases = 0
	}

	cb.state = state
	cb.transitionTime = time.Now()
	cb.window.Reset()
}

func (cb *circuitBreakerImpl) before() error {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == StateOpen && time.Since(cb.transitionTime) >= cb.config.WaitDurationInOpenState {
		cb.setStateUnsafe(StateHalfOpen)
	}

	switch cb.state {
	case StateOpen:
		return ErrOpenState
	case StateHalfOpen:
		if cb.halfOpenLeases <= 0 {
			return ErrHalfOpenState
		}
		cb.halfOpenLeases--
	default:
	}

	return nil
}

func (cb *circuitBreakerImpl) after(result any, err error, duration time.Duration) {
	isFailure := cb.shouldFailCall(result, err)
	isSlow := duration >= cb.config.SlowCallDurationThreshold

	var outcome CallOutcome
	if isFailure && isSlow {
		outcome = OutcomeSlowFailure
	} else if isFailure {
		outcome = OutcomeFailure
	} else if isSlow {
		outcome = OutcomeSlowSuccess
	} else {
		outcome = OutcomeSuccess
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.window.RecordOutcome(outcome)

	if cb.state == StateHalfOpen && !IsCallNotPermittedError(err) {
		cb.halfOpenCompletedLeases++
	}

	cb.evaluateStateTransitionUnsafe()
}

func (cb *circuitBreakerImpl) evaluateStateTransitionUnsafe() {
	switch cb.state {
	case StateClosed:
		if cb.window.Size() >= cb.config.MinimumNumberOfCalls && cb.areThresholdsExceededUnsafe() {
			cb.setStateUnsafe(StateOpen)
		}
	case StateHalfOpen:
		if cb.halfOpenCompletedLeases >= cb.config.PermittedNumberOfCallsInHalfOpenState {
			if cb.areThresholdsExceededUnsafe() {
				cb.setStateUnsafe(StateOpen)
			} else {
				cb.setStateUnsafe(StateClosed)
			}
		}
	default:
	}
}

func (cb *circuitBreakerImpl) areThresholdsExceededUnsafe() bool {
	_, failureRate, slowRate := cb.window.CallRates()
	return failureRate >= cb.config.FailureRateThreshold || slowRate >= cb.config.SlowCallRateThreshold
}

func (cb *circuitBreakerImpl) shouldFailCall(result any, err error) bool {
	if err != nil {
		if cb.config.FailOnErrorPredicate != nil && cb.config.FailOnErrorPredicate(err) {
			return true
		}

		for _, failErr := range cb.config.FailErrors {
			if errors.Is(err, failErr) {
				return true
			}
		}

		for _, ignoreErr := range cb.config.IgnoreErrors {
			if errors.Is(err, ignoreErr) {
				return false
			}
		}

		return true
	}

	if cb.config.FailOnResultPredicate != nil {
		return cb.config.FailOnResultPredicate(result)
	}

	return false
}
