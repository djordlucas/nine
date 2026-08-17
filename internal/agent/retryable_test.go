package agent

import (
	"errors"
	"fmt"
	"testing"

	"nine/internal/toolvm"
)

// stubRetryable is a minimal error satisfying the interface, so the contract is
// tested as the interface it is rather than through one concrete type.
type stubRetryable struct {
	msg              string
	retryable, state bool
}

func (e *stubRetryable) Error() string           { return e.msg }
func (e *stubRetryable) Retryable() (bool, bool) { return e.retryable, e.state }

func TestStatedRetryable(t *testing.T) {
	boolp := func(b bool) *bool { return &b }

	for _, tc := range []struct {
		name             string
		err              error
		wantRetry, wantS bool
	}{
		{"nil-ish plain error", errors.New("boom"), false, false},
		{"stub that said no", &stubRetryable{msg: "x", retryable: false, state: true}, false, true},
		{"stub that said yes", &stubRetryable{msg: "x", retryable: true, state: true}, true, true},
		{"stub that said nothing", &stubRetryable{msg: "x"}, false, false},
		{
			"a real sandboxed-tool error",
			&toolvm.CallError{Tool: "t", Message: "bad args", Detail: &toolvm.ErrorDetail{
				Retryable: boolp(false)}},
			false, true,
		},
		{
			"a sandboxed-tool error with no detail",
			&toolvm.CallError{Tool: "t", Message: "boom"},
			false, false,
		},
		{
			"wrapped, since dispatch wraps",
			fmt.Errorf("dispatch: %w", &toolvm.CallError{Tool: "t", Message: "bad args",
				Detail: &toolvm.ErrorDetail{Retryable: boolp(false)}}),
			false, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retry, stated := statedRetryable(tc.err)
			if retry != tc.wantRetry || stated != tc.wantS {
				t.Errorf("statedRetryable() = (%v, %v), want (%v, %v)",
					retry, stated, tc.wantRetry, tc.wantS)
			}
		})
	}
}
