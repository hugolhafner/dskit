package backoff

import (
	"time"
)

type Backoff interface {
	Next(attempt uint) time.Duration
}
