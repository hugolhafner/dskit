package backoff

import (
	"testing"
	"time"
)

func TestFixed_Next(t *testing.T) {
	tests := []struct {
		interval time.Duration
		attempt  uint
		expected time.Duration
	}{
		{interval: time.Second, attempt: 0, expected: time.Second},
		{interval: 500 * time.Millisecond, attempt: 5, expected: 500 * time.Millisecond},
		{interval: 2 * time.Second, attempt: 10, expected: 2 * time.Second},
	}

	for _, tt := range tests {
		fixed := NewFixed(tt.interval)
		result := fixed.Next(tt.attempt)
		if result != tt.expected {
			t.Errorf("Fixed.Next(%d) = %v; want %v", tt.attempt, result, tt.expected)
		}
	}
}

func BenchmarkNewFixed(b *testing.B) {
	for i := 0; i < b.N; i++ {
		NewFixed(time.Second)
	}
}

func BenchmarkFixed_Next(b *testing.B) {
	fixed := NewFixed(time.Second)
	for i := 0; i < b.N; i++ {
		fixed.Next(uint(i))
	}
}
