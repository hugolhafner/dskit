package backoff

import (
	"time"
)

type Linear struct {
	interval time.Duration
}

var _ Backoff = (*Linear)(nil)

func NewLinear(interval time.Duration) Linear {
	return Linear{
		interval: interval,
	}
}

func (l Linear) Next(attempt uint) time.Duration {
	return time.Duration(attempt) * l.interval
}
