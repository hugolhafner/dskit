package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/hugolhafner/dskit/backoff"
	"github.com/hugolhafner/dskit/retry"
)

func transientDataOperation(ctx context.Context) (string, error) {
	dur := time.Millisecond * 100 * time.Duration(rand.IntN(20))
	fmt.Printf("transient data duration: %v\n", dur)

	timer := time.NewTimer(dur)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	}

	return "data", nil
}

func main() {
	b := backoff.NewExponential(
		backoff.WithInitialInterval(100*time.Millisecond),
		backoff.WithMaxInterval(2*time.Second),
		backoff.WithMultiplier(2.0),
		backoff.WithJitter(0.2),
	)

	retry.SetGlobalMetrics(retry.NewInMemoryMetrics())
	p := retry.MustNewPolicy(
		"simple-retry-policy",
		retry.WithAttemptTimeout(time.Second),
		retry.WithMaxAttempts(3),
		retry.WithBackoff(b),
	)

	for i := 0; i < 5; i++ {
		data, err := retry.Execute(context.Background(), p, transientDataOperation)

		if err != nil {
			if err, ok := retry.AsRetryError(err); ok {
				fmt.Println(err.Verbose())
			} else {
				fmt.Println("operation failed:", err)
			}
		} else {
			fmt.Println("operation succeeded")
			fmt.Println("data:", data)
		}
	}

	fmt.Println("Metrics:", retry.GetGlobalMetrics().(*retry.InMemoryMetrics).GetMetrics())
}
