package runtime

import (
	"encoding/json"
	"testing"
)

// A process write is never inert under require_approval = "on_capability":
// it runs until stopped, whatever it declares. requestsProcess is what the
// gate keys on, so it has to be right about the shapes tool_write sends.
func TestRequestsProcess(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"absent", `{"name":"x","source":"y"}`, false},
		{"explicit null", `{"name":"x","process":null}`, false},
		{"empty object still asks", `{"name":"x","process":{}}`, true},
		{"with a clock", `{"name":"x","process":{"every":"30m"}}`, true},
		// Fail closed: arguments we cannot read are treated as a process, so a
		// malformed write cannot slip past the gate.
		{"unparseable fails closed", `{not json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestsProcess(json.RawMessage(tc.args)); got != tc.want {
				t.Fatalf("requestsProcess(%s) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
