package api

import "testing"

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost":    true,
		"LocalHost":    true,
		" localhost ":  true,
		"127.0.0.1":    true,
		"127.0.0.53":   true,
		"::1":          true,
		"[::1]":        true,
		"0.0.0.0":      false,
		"::":           false,
		"":             false,
		"192.168.1.10": false,
		"example.com":  false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestWarnIfExposed pins when the warning fires. A token silences it at any
// address; loopback silences it with no token.
func TestWarnIfExposed(t *testing.T) {
	cases := []struct {
		host, token string
		wantWarn    bool
	}{
		{"0.0.0.0", "", true},
		{"::", "", true},
		{"192.168.1.10", "", true},
		{"0.0.0.0", "secret", false},
		{"localhost", "", false},
		{"127.0.0.1", "", false},
	}
	for _, tc := range cases {
		exposed := tc.token == "" && !isLoopbackHost(tc.host)
		if exposed != tc.wantWarn {
			t.Errorf("host=%q token=%q exposed=%v, want %v", tc.host, tc.token, exposed, tc.wantWarn)
		}
		warnIfExposed(tc.host, tc.token) // must not panic
	}
}
