package retry_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hugolhafner/dskit/retry"
)

func TestRetryError_Error(t *testing.T) {
	baseTime := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		err      *retry.RetryError
		expected string
	}{
		{
			name:     "no attempts",
			err:      &retry.RetryError{},
			expected: "retry failed: no attempts recorded",
		},
		{
			name: "single attempt",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Timestamp: baseTime, Duration: time.Second, Error: errors.New("connection refused")},
				},
			},
			expected: "retry failed after 1 attempt(s): connection refused",
		},
		{
			name: "multiple attempts reports last error",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Timestamp: baseTime, Error: errors.New("first error")},
					{Number: 2, Timestamp: baseTime.Add(time.Second), Error: errors.New("second error")},
					{Number: 3, Timestamp: baseTime.Add(2 * time.Second), Error: errors.New("final error")},
				},
			},
			expected: "retry failed after 3 attempt(s): final error",
		},
		{
			name: "nil error in last attempt",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Error: nil},
				},
			},
			expected: "retry failed after 1 attempt(s): <nil>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.err.Error(); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestRetryError_Unwrap(t *testing.T) {
	baseErr := errors.New("base error")
	terminationErr := errors.New("termination error")
	wrappedErr := fmt.Errorf("wrapped: %w", baseErr)

	tests := []struct {
		name     string
		err      *retry.RetryError
		expected error
	}{
		{
			name:     "no attempts and no termination error",
			err:      &retry.RetryError{},
			expected: nil,
		},
		{
			name: "termination error takes precedence",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Error: baseErr},
				},
				TerminationError: terminationErr,
			},
			expected: terminationErr,
		},
		{
			name: "returns last attempt error when no termination error",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Error: errors.New("first")},
					{Number: 2, Error: baseErr},
				},
			},
			expected: baseErr,
		},
		{
			name: "preserves wrapped error chain",
			err: &retry.RetryError{
				Attempts: []retry.Attempt{
					{Number: 1, Error: wrappedErr},
				},
			},
			expected: wrappedErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Unwrap()
			if got != tt.expected {
				t.Errorf("got %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestRetryError_UnwrapChain(t *testing.T) {
	innerErr := errors.New("inner error")
	wrappedErr := fmt.Errorf("outer: %w", innerErr)

	retryErr := &retry.RetryError{
		Attempts: []retry.Attempt{
			{Number: 1, Error: wrappedErr},
		},
	}

	if !errors.Is(retryErr, innerErr) {
		t.Error("errors.Is should find inner error through unwrap chain")
	}
}

func TestRetryError_Last(t *testing.T) {
	expectedErr := errors.New("last error")
	retryErr := &retry.RetryError{
		Attempts: []retry.Attempt{
			{Number: 1, Error: errors.New("first")},
			{Number: 2, Error: expectedErr},
		},
	}

	if got := retryErr.Last(); got != expectedErr {
		t.Errorf("Last() = %v, want %v", got, expectedErr)
	}
}

func TestRetryError_All(t *testing.T) {
	tests := []struct {
		name         string
		attempts     []retry.Attempt
		expectedLen  int
		expectedErrs []string
	}{
		{
			name:         "empty attempts",
			attempts:     []retry.Attempt{},
			expectedLen:  0,
			expectedErrs: []string{},
		},
		{
			name: "collects all errors in order",
			attempts: []retry.Attempt{
				{Number: 1, Error: errors.New("error one")},
				{Number: 2, Error: errors.New("error two")},
				{Number: 3, Error: errors.New("error three")},
			},
			expectedLen:  3,
			expectedErrs: []string{"error one", "error two", "error three"},
		},
		{
			name: "handles nil errors",
			attempts: []retry.Attempt{
				{Number: 1, Error: nil},
				{Number: 2, Error: errors.New("real error")},
			},
			expectedLen:  2,
			expectedErrs: []string{"", "real error"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retryErr := &retry.RetryError{Attempts: tt.attempts}
			got := retryErr.All()

			if len(got) != tt.expectedLen {
				t.Fatalf("All() returned %d errors, want %d", len(got), tt.expectedLen)
			}

			for i, err := range got {
				errStr := ""
				if err != nil {
					errStr = err.Error()
				}
				if errStr != tt.expectedErrs[i] {
					t.Errorf("All()[%d] = %q, want %q", i, errStr, tt.expectedErrs[i])
				}
			}
		})
	}
}

func TestRetryError_Verbose(t *testing.T) {
	baseTime := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

	retryErr := &retry.RetryError{
		Attempts: []retry.Attempt{
			{Number: 1, Timestamp: baseTime, Duration: 50 * time.Millisecond, Error: errors.New("timeout")},
			{Number: 2, Timestamp: baseTime.Add(time.Second), Duration: 100 * time.Millisecond,
				Error: errors.New("connection reset")},
		},
	}

	verbose := retryErr.Verbose()

	expectedSubstrings := []string{
		"retry failed after 2 attempt(s):",
		"attempt 1",
		"2024-06-15T10:30:00Z",
		"50ms",
		"timeout",
		"attempt 2",
		"2024-06-15T10:30:01Z",
		"100ms",
		"connection reset",
	}

	for _, substr := range expectedSubstrings {
		if !strings.Contains(verbose, substr) {
			t.Errorf("Verbose() missing expected substring %q\ngot: %s", substr, verbose)
		}
	}
}

func TestAsRetryError(t *testing.T) {
	retryErr := &retry.RetryError{
		Attempts: []retry.Attempt{{Number: 1, Error: errors.New("test")}},
	}
	wrappedRetryErr := fmt.Errorf("context: %w", retryErr)

	tests := []struct {
		name       string
		err        error
		wantOk     bool
		wantNonNil bool
	}{
		{
			name:       "nil error",
			err:        nil,
			wantOk:     false,
			wantNonNil: false,
		},
		{
			name:       "non-retry error",
			err:        errors.New("regular error"),
			wantOk:     false,
			wantNonNil: false,
		},
		{
			name:       "direct retry error",
			err:        retryErr,
			wantOk:     true,
			wantNonNil: true,
		},
		{
			name:       "wrapped retry error",
			err:        wrappedRetryErr,
			wantOk:     true,
			wantNonNil: true,
		},
		{
			name:       "double wrapped retry error",
			err:        fmt.Errorf("outer: %w", wrappedRetryErr),
			wantOk:     true,
			wantNonNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := retry.AsRetryError(tt.err)

			if ok != tt.wantOk {
				t.Errorf("Asretry.RetryError() ok = %v, want %v", ok, tt.wantOk)
			}

			if tt.wantNonNil && got == nil {
				t.Error("Asretry.RetryError() returned nil, want non-nil")
			}

			if !tt.wantNonNil && got != nil {
				t.Errorf("Asretry.RetryError() returned %v, want nil", got)
			}
		})
	}
}

func TestValidationError_Error(t *testing.T) {
	tests := []struct {
		name     string
		field    string
		message  string
		expected string
	}{
		{
			name:     "standard validation error",
			field:    "MaxRetries",
			message:  "must be positive",
			expected: "Policy error: field 'MaxRetries' - must be positive",
		},
		{
			name:     "empty field name",
			field:    "",
			message:  "is required",
			expected: "Policy error: field '' - is required",
		},
		{
			name:     "empty message",
			field:    "Timeout",
			message:  "",
			expected: "Policy error: field 'Timeout' - ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &retry.ValidationError{Field: tt.field, Message: tt.message}
			if got := err.Error(); got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestIsValidationError(t *testing.T) {
	validationErr := &retry.ValidationError{Field: "test", Message: "invalid"}
	wrappedValidation := fmt.Errorf("config failed: %w", validationErr)

	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{
			name:   "nil error",
			err:    nil,
			expect: false,
		},
		{
			name:   "regular error",
			err:    errors.New("not a validation error"),
			expect: false,
		},
		{
			name:   "direct validation error",
			err:    validationErr,
			expect: true,
		},
		{
			name:   "wrapped validation error",
			err:    wrappedValidation,
			expect: true,
		},
		{
			name:   "retry error is not validation error",
			err:    &retry.RetryError{},
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retry.IsValidationError(tt.err); got != tt.expect {
				t.Errorf("IsValidationError() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestIsResultPredicateRetry(t *testing.T) {
	wrappedPredicate := fmt.Errorf("check failed: %w", retry.ErrResultPredicateRetry)

	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{
			name:   "nil error",
			err:    nil,
			expect: false,
		},
		{
			name:   "unrelated error",
			err:    errors.New("something else"),
			expect: false,
		},
		{
			name:   "direct predicate error",
			err:    retry.ErrResultPredicateRetry,
			expect: true,
		},
		{
			name:   "wrapped predicate error",
			err:    wrappedPredicate,
			expect: true,
		},
		{
			name:   "deeply wrapped predicate error",
			err:    fmt.Errorf("level2: %w", fmt.Errorf("level1: %w", retry.ErrResultPredicateRetry)),
			expect: true,
		},
		{
			name:   "similar message but different error",
			err:    errors.New("result predicate triggered retry"),
			expect: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retry.IsResultPredicateRetry(tt.err); got != tt.expect {
				t.Errorf("IsResultPredicateRetry() = %v, want %v", got, tt.expect)
			}
		})
	}
}

func TestRetryError_ImplementsErrorInterface(t *testing.T) {
	var _ error = (*retry.RetryError)(nil)
}

func TestValidationError_ImplementsErrorInterface(t *testing.T) {
	var _ error = (*retry.ValidationError)(nil)
}

func TestRetryError_ErrorsAs(t *testing.T) {
	original := &retry.RetryError{
		Attempts: []retry.Attempt{{Number: 1, Error: errors.New("test")}},
	}
	wrapped := fmt.Errorf("operation failed: %w", original)

	var target *retry.RetryError
	if !errors.As(wrapped, &target) {
		t.Fatal("errors.As should match wrapped retry.RetryError")
	}

	if len(target.Attempts) != 1 {
		t.Errorf("extracted error has %d attempts, want 1", len(target.Attempts))
	}
}

func TestRetryError_ErrorsIs(t *testing.T) {
	sentinel := errors.New("sentinel error")
	retryErr := &retry.RetryError{
		Attempts: []retry.Attempt{
			{Number: 1, Error: fmt.Errorf("wrapped: %w", sentinel)},
		},
	}

	if !errors.Is(retryErr, sentinel) {
		t.Error("errors.Is should find sentinel through retry.RetryError.Unwrap chain")
	}
}

func TestRetryError_TerminationErrorPrecedence(t *testing.T) {
	sentinel := errors.New("termination sentinel")
	attemptErr := errors.New("attempt error")

	retryErr := &retry.RetryError{
		Attempts:         []retry.Attempt{{Number: 1, Error: attemptErr}},
		TerminationError: sentinel,
	}

	if !errors.Is(retryErr, sentinel) {
		t.Error("errors.Is should find termination error")
	}

	if errors.Is(retryErr, attemptErr) {
		t.Error("errors.Is should not find attempt error when termination error is set")
	}
}
