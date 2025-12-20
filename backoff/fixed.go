package backoff

import (
	"time"
)

var _ Backoff = (*Fixed)(nil)

type Fixed struct {
	interval time.Duration
}

func NewFixed(d time.Duration) Fixed {
	return Fixed{
		interval: d,
	}
}

func (f Fixed) Next(_ uint) time.Duration {
	return f.interval
}
