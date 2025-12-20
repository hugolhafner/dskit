package retry

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrResultPredicateRetry is returned when the result predicate triggers a retry
var ErrResultPredicateRetry = errors.New("result predicate triggered retry")

// RetryError contains the complete retry history
type RetryError struct {
	Attempts         []Attempt
	TerminationError error
}

func (e *RetryError) Error() string {
	if len(e.Attempts) == 0 {
		return "retry failed: no attempts recorded"
	}

	last := e.Attempts[len(e.Attempts)-1]
	return fmt.Sprintf("retry failed after %d attempt(s): %v", len(e.Attempts), last.Error)
}

// Unwrap returns the last error for compatibility with errors.Is/As
func (e *RetryError) Unwrap() error {
	if e.TerminationError != nil {
		return e.TerminationError
	}
	if len(e.Attempts) == 0 {
		return nil
	}
	return e.Attempts[len(e.Attempts)-1].Error
}

// Last returns the final error that caused the retry to fail
func (e *RetryError) Last() error {
	return e.Unwrap()
}

// All returns all errors from all attempts
func (e *RetryError) All() []error {
	errs := make([]error, len(e.Attempts))
	for i, a := range e.Attempts {
		errs[i] = a.Error
	}
	return errs
}

// Verbose returns a full retry history error string
func (e *RetryError) Verbose() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("retry failed after %d attempt(s):\n", len(e.Attempts)))

	for _, a := range e.Attempts {
		sb.WriteString(fmt.Sprintf(
			"  attempt %d [%s] (took %v): %v\n",
			a.Number,
			a.Timestamp.Format(time.RFC3339),
			a.Duration,
			a.Error,
		))
	}
	return sb.String()
}

// AsRetryError checks if the given error is a retry RetryError
func AsRetryError(err error) (*RetryError, bool) {
	var e *RetryError
	ok := errors.As(err, &e)
	return e, ok
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return "Policy error: field '" + e.Field + "' - " + e.Message
}

func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

func IsResultPredicateRetry(err error) bool {
	return errors.Is(err, ErrResultPredicateRetry)
}
