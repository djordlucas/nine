package runtime

import (
	"encoding/json"
	"testing"
)

// A standing promotion must always reach a human — including under
// require_approval = "never", which is the one place that setting does not mean
// what it says. requestsStanding is what the gate keys on, so it has to be right
// about the shapes tool_write actually sends.
func TestRequestsStanding(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"absent", `{"name":"x","source":"y"}`, false},
		{"explicit null", `{"name":"x","standing":null}`, false},
		{"empty object still asks", `{"name":"x","standing":{}}`, true},
		{"with a cadence", `{"name":"x","standing":{"interval":"10s"}}`, true},
		// Fail closed: arguments we cannot read are treated as a promotion, so a
		// malformed write cannot slip past the one gate that always fires.
		{"unparseable fails closed", `{not json`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestsStanding(json.RawMessage(tc.args)); got != tc.want {
				t.Fatalf("requestsStanding(%s) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}
