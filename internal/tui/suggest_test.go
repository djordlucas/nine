package tui

import (
	"errors"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// The picker filters by prefix on the command name, case-insensitively. A bare
// "/" offers the whole catalog.
func TestMatchCmds(t *testing.T) {
	names := func(cs []slashCmd) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.name
		}
		return out
	}

	if got := matchCmds(""); len(got) != len(slashCmds) {
		t.Errorf("matchCmds(%q) returned %d commands, want the whole catalog (%d)", "", len(got), len(slashCmds))
	}
	if got := names(matchCmds("t")); strings.Join(got, ",") != "tools,think" {
		t.Errorf("matchCmds(%q) = %v, want tools and think in catalog order", "t", got)
	}
	if got := names(matchCmds("tool")); strings.Join(got, ",") != "tools" {
		t.Errorf("matchCmds(%q) = %v, want just tools", "tool", got)
	}
	if got := names(matchCmds("TOOL")); strings.Join(got, ",") != "tools" {
		t.Errorf("matchCmds(%q) = %v, want matching to ignore case", "TOOL", got)
	}
	if got := matchCmds("zz"); len(got) != 0 {
		t.Errorf("matchCmds(%q) = %v, want no matches", "zz", got)
	}
}

// suggestPrefix decides whether the input is still spelling a command name.
// Typing an argument (anything past the first space) hands the line back to the
// user, and text that does not start with "/" is an ordinary message.
func TestSuggestPrefix(t *testing.T) {
	cases := []struct {
		value      string
		wantPrefix string
		wantOK     bool
	}{
		{"", "", false},
		{"hello", "", false},
		{"say /help", "", false},
		{"/", "", true},
		{"/to", "to", true},
		{"/tools", "tools", true},
		{"/tools ", "", false},
		{"/tools http", "", false},
	}
	for _, tc := range cases {
		prefix, ok := suggestPrefix(tc.value)
		if ok != tc.wantOK || prefix != tc.wantPrefix {
			t.Errorf("suggestPrefix(%q) = (%q, %v), want (%q, %v)", tc.value, prefix, ok, tc.wantPrefix, tc.wantOK)
		}
	}
}

// newTestModel builds just enough of the model to exercise the picker without a
// terminal: the input box, the list, and a viewport marked ready.
func newTestModel() *model {
	pal := darkPalette()
	m := initialModel("/tmp/nine-test.sock", "nine", "", pal, "dark", false, nil, "test")
	m.display.width = 80
	m.display.height = 24
	return &m
}

// Typing drives the picker: "/" opens it on the full catalog, more characters
// narrow it, and deleting back past the "/" closes it. A prefix that matches
// nothing closes it too, rather than leaving an empty box on screen.
func TestSuggestionsOpenAndCloseWhileTyping(t *testing.T) {
	m := newTestModel()

	cases := []struct {
		value    string
		wantOpen bool
		wantRows int
	}{
		{"", false, 0},
		{"/", true, len(slashCmds)},
		{"/t", true, 2},
		{"/to", true, 1},
		{"/tools", true, 1},
		{"/tools ", false, 0}, // argument being typed — the picker steps aside
		{"/zz", false, 0},     // nothing matches
		{"/", true, len(slashCmds)},
		{"", false, 0}, // the "/" was deleted
	}
	for _, tc := range cases {
		m.chat.input.SetValue(tc.value)
		m.refreshSuggestions()

		if m.chat.suggestOpen != tc.wantOpen {
			t.Errorf("input %q: suggestOpen = %v, want %v", tc.value, m.chat.suggestOpen, tc.wantOpen)
		}
		if !tc.wantOpen {
			continue
		}
		if n := len(m.chat.suggest.Items()); n != tc.wantRows {
			t.Errorf("input %q: %d items, want %d", tc.value, n, tc.wantRows)
		}
		if got := m.chat.suggest.Index(); got != 0 {
			t.Errorf("input %q: selection at %d, want the first match", tc.value, got)
		}
	}
}

// While a question is on screen the input is a free-text answer box, so a
// leading "/" is just a character and must not pop the picker open.
func TestSuggestionsSuppressedWhileAnswering(t *testing.T) {
	m := newTestModel()
	m.chat.humanQueue = append(m.chat.humanQueue, req("r1", "which one?", ""))

	m.chat.input.SetValue("/tools")
	m.refreshSuggestions()
	if m.chat.suggestOpen {
		t.Error("picker opened over a pending question; the input is an answer box there")
	}

	// Answering releases the input, and the picker behaves normally again.
	m.chat.popHuman()
	m.refreshSuggestions()
	if !m.chat.suggestOpen {
		t.Error("picker stayed shut after the question was answered")
	}
}

// The picker borrows its rows from the transcript, which must get them back
// when it closes — otherwise the viewport shrinks a little on every command.
func TestSuggestionsBorrowViewportRows(t *testing.T) {
	m := newTestModel()
	m.chat.ready = true

	base := m.viewportHeight()

	m.chat.input.SetValue("/t")
	m.refreshSuggestions()
	if got, want := m.viewportHeight(), base-2; got != want {
		t.Errorf("viewport height with 2 suggestions = %d, want %d", got, want)
	}

	m.chat.input.SetValue("")
	m.refreshSuggestions()
	if got := m.viewportHeight(); got != base {
		t.Errorf("viewport height after closing = %d, want the original %d", got, base)
	}
}

// The catalog never shows more rows than it is allowed, however many commands
// match.
func TestSuggestionsClampToMaxRows(t *testing.T) {
	m := newTestModel()
	m.chat.input.SetValue("/")
	m.refreshSuggestions()

	if len(slashCmds) <= maxSuggestRows {
		t.Skipf("catalog has %d commands, at or under the %d-row cap", len(slashCmds), maxSuggestRows)
	}
	if got := m.suggestHeight(); got != maxSuggestRows {
		t.Errorf("picker height = %d, want it clamped to %d", got, maxSuggestRows)
	}
}

// Accepting a command fills the input and dismisses the picker. Commands that
// take an argument get a trailing space so the argument can be typed straight
// away; commands that do not are left ready to submit.
func TestAcceptSuggestion(t *testing.T) {
	if got := (slashCmd{name: "help"}).completion(); got != "/help" {
		t.Errorf("completion for a no-argument command = %q, want %q", got, "/help")
	}
	if got := (slashCmd{name: "tools", args: "[filter]"}).completion(); got != "/tools " {
		t.Errorf("completion for an argument-taking command = %q, want %q", got, "/tools ")
	}

	m := newTestModel()
	m.chat.input.SetValue("/too")
	m.refreshSuggestions()

	c, ok := m.selectedSuggestion()
	if !ok || c.name != "tools" {
		t.Fatalf("selected = %+v (ok=%v), want tools", c, ok)
	}
	m.acceptSuggestion(c)

	if got := m.chat.input.Value(); got != "/tools " {
		t.Errorf("input after accepting = %q, want %q", got, "/tools ")
	}
	if m.chat.suggestOpen {
		t.Error("picker stayed open after a command was accepted")
	}
	if _, ok := m.selectedSuggestion(); ok {
		t.Error("a closed picker still reports a selection")
	}
}

// Arrow keys move the highlight, and it never runs off either end of the list.
func TestSuggestionCursorMovement(t *testing.T) {
	m := newTestModel()
	m.chat.input.SetValue("/t")
	m.refreshSuggestions()

	m.chat.suggest.CursorUp() // already at the top
	if got := m.chat.suggest.Index(); got != 0 {
		t.Errorf("index after up from the top = %d, want 0", got)
	}
	m.chat.suggest.CursorDown()
	if got := m.chat.suggest.Index(); got != 1 {
		t.Errorf("index after down = %d, want 1", got)
	}
	if c, _ := m.selectedSuggestion(); c.name != "think" {
		t.Errorf("selected = %q, want think", c.name)
	}
}

// A row shows the command on the left and its description on the right.
func TestRenderSuggestRow(t *testing.T) {
	pal := darkPalette()
	c := slashCmd{name: "tools", args: "[filter]", desc: "list all tools (optional name filter)"}

	row := renderSuggestRow(pal, 12, 80, c, false)
	if !strings.Contains(row, "/tools") {
		t.Errorf("row = %q, want the command name", row)
	}
	if !strings.Contains(row, c.desc) {
		t.Errorf("row = %q, want the description", row)
	}

	// The selection marker is a multi-byte rune, so padding an unselected row by
	// len() rather than cell width would shift every name to the right.
	if sel, unsel := lipgloss.Width(renderSuggestRow(pal, 12, 80, c, true)), lipgloss.Width(row); sel != unsel {
		t.Errorf("selected row is %d cells wide, unselected is %d — the name column must not shift", sel, unsel)
	}

	// A narrow terminal truncates the description rather than wrapping the row
	// onto a second line, which would break the picker's height accounting.
	narrow := renderSuggestRow(pal, 12, 30, c, false)
	if strings.Contains(narrow, "\n") {
		t.Errorf("narrow row = %q, want a single line", narrow)
	}
	if strings.Contains(narrow, c.desc) {
		t.Errorf("narrow row = %q, want the description truncated", narrow)
	}
}

// Every command the picker offers must actually be dispatched somewhere, or the
// user picks it from a list and gets "unknown command" back.
func TestCatalogCommandsAreDispatched(t *testing.T) {
	// /new, /clear and /think need the TUI's own state (the session, the
	// transcript, the turn), so they are handled in tui.go's key handler rather
	// than by runCmd.
	tuiLocal := map[string]bool{"new": true, "clear": true, "think": true}

	for _, c := range slashCmds {
		if tuiLocal[c.name] {
			continue
		}
		// The handlers dereference a nil client, which is fine: this asserts
		// only that the switch dispatches the name, not what the handler does.
		func() {
			defer func() { _ = recover() }()
			if _, err := runCmd(c.name, "", nil, nil, ""); errors.Is(err, errUnknownCmd) {
				t.Errorf("/%s is in the catalog but runCmd does not dispatch it", c.name)
			}
		}()
	}

	if _, err := runCmd("definitely-not-a-command", "", nil, nil, ""); !errors.Is(err, errUnknownCmd) {
		t.Errorf("unknown command error = %v, want it to wrap errUnknownCmd", err)
	}
}

// /help is generated from the catalog, so the two can never drift.
func TestHelpListsEveryCommand(t *testing.T) {
	help := cmdHelp()
	for _, c := range slashCmds {
		if !strings.Contains(help, c.label()) {
			t.Errorf("/help is missing %q", c.label())
		}
		if !strings.Contains(help, c.desc) {
			t.Errorf("/help is missing the description for %q", c.label())
		}
	}
}

// Guard the assumption the picker's layout rests on: descriptions are short
// enough to fit beside the widest command name in an 80-column terminal.
func TestDescriptionsFitEightyColumns(t *testing.T) {
	nameWidth := 0
	for _, c := range slashCmds {
		if n := len(c.name) + 1; n > nameWidth {
			nameWidth = n
		}
	}
	const width = 80
	for _, c := range slashCmds {
		if got := 2 + nameWidth + 2 + len(c.desc); got > width {
			t.Errorf("/%s: row needs %d columns, want at most %d — shorten the description", c.name, got, width)
		}
	}
}
