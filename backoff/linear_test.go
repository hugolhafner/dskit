package backoff

import (
	"testing"
	"time"
)

func TestLinear_Next(t *testing.T) {
	tests := []struct {
		interval time.Duration
		attempt  uint
		expected time.Duration
	}{
		{interval: time.Second, attempt: 0, expected: 0 * time.Second},
		{interval: time.Second, attempt: 1, expected: 1 * time.Second},
		{interval: time.Second, attempt: 2, expected: 2 * time.Second},
		{interval: 500 * time.Millisecond, attempt: 3, expected: 1500 * time.Millisecond},
		{interval: 2 * time.Second, attempt: 5, expected: 10 * time.Second},
	}

	for _, tt := range tests {
		l := NewLinear(tt.interval)
		result := l.Next(tt.attempt)
		if result != tt.expected {
			t.Errorf("Linear.Next(%d) with interval %v = %v; want %v", tt.attempt, tt.interval, result, tt.expected)
		}
	}
}
