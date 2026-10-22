package runtime

import "testing"

func TestGoalFolder(t *testing.T) {
	cases := []struct{ id, description, want string }{
		{"1eb9e2c3-8630-4a71-875b-01d5e1d674bb", "Keep yourself updated on news about Canada", "goals/keep-yourself-updated-on-news-about-cana-1eb9e2c3/"},
		{"3564d21b-7824-4aa1-ba8d-20ff335108fe", "JavaScript & WASM: news!", "goals/javascript-wasm-news-3564d21b/"},
		{"g1", "Été — café", "goals/t-caf-g1/"},
		{"9f13b80d-4e15-4bd1-a5cc-6776ee51aae4", "!!!", "goals/9f13b80d/"},
		{"tidy-notes", "Keep notes tidy", "goals/keep-notes-tidy-tidy-notes/"},
	}
	for _, c := range cases {
		if got := GoalFolder(c.id, c.description); got != c.want {
			t.Errorf("GoalFolder(%q, %q) = %q, want %q", c.id, c.description, got, c.want)
		}
	}
}
