package circuitbreaker

type CallOutcome int

const (
	OutcomeSuccess CallOutcome = iota
	OutcomeFailure
	OutcomeSlowSuccess
	OutcomeSlowFailure
)

type Window interface {
	Size() int

	RecordOutcome(CallOutcome)

	// CallRates returns the success rate, failure rate, and slow call rate in percentage.
	CallRates() (float64, float64, float64)

	Reset()
}
