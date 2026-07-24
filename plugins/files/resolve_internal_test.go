package main

import "testing"

// TestRootRelative pins the path mapping the plugin applies inside a workspace:
// the /work alias and relative paths land under the root, genuine outside
// absolute paths pass through untouched, and .. cannot climb above the root.
func TestRootRelative(t *testing.T) {
	const root = "/ws"
	cases := []struct {
		in, want string
	}{
		{"/work/output.txt", "/ws/output.txt"},    // alias → root
		{"/work/a/b.txt", "/ws/a/b.txt"},          // nested under alias
		{"/work", "/ws"},                          // bare alias is the root
		{"notes.txt", "/ws/notes.txt"},            // relative → root
		{"a/b.txt", "/ws/a/b.txt"},                // nested relative
		{"/etc/hosts", "/etc/hosts"},              // real absolute path untouched
		{"/workshop/x", "/workshop/x"},            // not the alias, left as-is
		{"/work/../escape.txt", "/ws/escape.txt"}, // .. cannot climb past root
	}
	for _, c := range cases {
		if got := rootRelative(root, c.in); got != c.want {
			t.Errorf("rootRelative(%q, %q) = %q, want %q", root, c.in, got, c.want)
		}
	}
}
