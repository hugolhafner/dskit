package main

//
// import (
// 	"context"
// 	"time"
//
// 	"github.com/hugolhafner/dskit/backoff"
// 	"github.com/hugolhafner/dskit/retry"
// )
//
// func exampleSlowCall(ctx context.Context) error {
// 	// Simulate a slow call
// 	time.Sleep(2 * time.Second)
// 	return nil
// }
//
// func CircuitBreakerRetryExample() {
// 	r := retry.MustNewPolicy(
// 		retry.WithBackoff(backoff.NewExponential()))
//
// 	cb := circuitbreaker.MustNew(
// 		circuitbreaker.WithRetryPolicy(r),
// 		circuitbreaker.WithFailureThreshold(5),
// 		circuitbreaker.WithResetTimeout(30*time.Second),
// 	)
//
// 	err := cb.DoCtx(ctx, exampleSlowCall)
// 	if err != nil {
// 		// Handle error
// 	}
// }
