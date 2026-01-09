package retry

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPolicy(t *testing.T) {
	tests := []struct {
		name  string
		opts  []Option
		check func(policy *Policy, err error) error
	}{
		{
			name: "Test ignoreErrors merge",
			opts: []Option{
				WithIgnoreErrors(errors.New("error1")),
				WithIgnoreErrors(errors.New("error2")),
			},
			check: func(policy *Policy, _ error) error {
				if len(policy.ignoreErrors) != 2 {
					fmt.Println(policy.ignoreErrors)
					return errors.New("expected 2 ignore errors")
				}
				return nil
			},
		},
		{
			name: "Test retryErrors merge",
			opts: []Option{
				WithRetryErrors(errors.New("error1")),
				WithRetryErrors(errors.New("error2")),
			},
			check: func(policy *Policy, _ error) error {
				if len(policy.retryErrors) != 2 {
					return errors.New("expected 2 retry errors")
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := NewPolicy("test.Policy", tt.opts...)
			require.NoError(t, tt.check(policy, err))
		})
	}
}
