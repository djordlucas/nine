package config

import "testing"

// The scope check is the one that matters: the two scopes differ in whether a
// tool can carry data between conversations, so an operator who did not decide
// must be told rather than defaulted.
func TestValidateStateGrantScope(t *testing.T) {
	cases := []struct {
		name    string
		grant   *ToolStateGrant
		wantErr bool
	}{
		{"absent grant", nil, false},
		{"tool scope", &ToolStateGrant{Scope: "tool"}, false},
		{"conversation scope", &ToolStateGrant{Scope: "conversation"}, false},
		{"no scope", &ToolStateGrant{}, true},
		{"unknown scope", &ToolStateGrant{Scope: "session"}, true},
		{"negative quota", &ToolStateGrant{Scope: "tool", MaxKeys: -1}, true},
		{"bad ttl", &ToolStateGrant{Scope: "tool", TTL: "forever"}, true},
		{"zero ttl", &ToolStateGrant{Scope: "tool", TTL: "0s"}, true},
		{"good ttl", &ToolStateGrant{Scope: "tool", TTL: "24h"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStateGrant("tool.x", tc.grant)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// An unset scope must explain itself rather than just refusing: the operator has
// to be able to tell which value they want.
func TestUnsetStateScopeExplainsTheChoice(t *testing.T) {
	err := validateStateGrant("tool.x", &ToolStateGrant{})
	if err == nil {
		t.Fatal("an unset scope was accepted")
	}
	for _, want := range []string{"tool", "conversation", "between conversations"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
