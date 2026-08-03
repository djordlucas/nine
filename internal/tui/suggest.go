package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// maxSuggestRows caps how tall the slash-command picker grows. It borrows its
// rows from the transcript viewport, so it stays small enough to leave the
// conversation readable while it is open.
const maxSuggestRows = 8

// cmdItem adapts a catalog entry to bubbles' list.Item.
type cmdItem struct{ slashCmd }

func (i cmdItem) FilterValue() string { return "/" + i.name }

// cmdDelegate draws one command per row: the name in a fixed-width left column
// so every description lines up, the description dimmed to its right.
type cmdDelegate struct {
	pal       palette
	nameWidth int // widest "/name" in the catalog, so the columns align
}

func (d cmdDelegate) Height() int                         { return 1 }
func (d cmdDelegate) Spacing() int                        { return 0 }
func (d cmdDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d cmdDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	it, ok := item.(cmdItem)
	if !ok {
		return
	}
	fmt.Fprint(w, renderSuggestRow(d.pal, d.nameWidth, m.Width(), it.slashCmd, index == m.Index()))
}

// renderSuggestRow lays out a single picker row. Split out from Render so it can
// be tested without standing up a list.Model.
func renderSuggestRow(pal palette, nameWidth, width int, c slashCmd, selected bool) string {
	// The marker is one multi-byte rune plus a space: two cells, but four bytes.
	// Unselected rows pad by cell width, not len(), or every name shifts right.
	const marker, markerWidth = "› ", 2
	name := fmt.Sprintf("%-*s", nameWidth, "/"+c.name)

	desc := c.desc
	if room := width - markerWidth - nameWidth - 2; room > 1 && len(desc) > room {
		desc = desc[:room-1] + "…"
	}

	if selected {
		return pal.arrow.Render(marker) + pal.tool.Render(name) + "  " + pal.output.Render(desc)
	}
	return strings.Repeat(" ", markerWidth) + pal.continuation.Render(name) + "  " + pal.ts.Render(desc)
}

// newSuggestList builds the picker's list model with every chrome element the
// popup does not want switched off. Filtering in particular: the list's own
// filter is a modal prompt bound to "/" — the very key that opens this picker —
// so the picker filters by rebuilding its items instead (see
// model.refreshSuggestions).
func newSuggestList(pal palette) list.Model {
	nameWidth := 0
	for _, c := range slashCmds {
		if n := len(c.name) + 1; n > nameWidth {
			nameWidth = n
		}
	}
	l := list.New(nil, cmdDelegate{pal: pal, nameWidth: nameWidth}, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(false)
	l.SetShowFilter(false)
	l.SetFilteringEnabled(false)
	l.DisableQuitKeybindings() // ctrl+c must always reach the TUI's own handler
	return l
}

// suggestPrefix reports the command prefix the input currently spells, and
// whether the picker should consider opening at all. It wants a leading slash
// and no argument yet: once a space is typed the user has moved on to the
// argument and the picker gets out of the way.
func suggestPrefix(value string) (string, bool) {
	if !strings.HasPrefix(value, "/") {
		return "", false
	}
	rest := value[1:]
	if strings.ContainsAny(rest, " \t") {
		return "", false
	}
	return rest, true
}

// suggestHeight is how many rows the picker occupies, and so how many rows the
// viewport gives up while it is open.
func (m *model) suggestHeight() int {
	if !m.chat.suggestOpen {
		return 0
	}
	n := len(m.chat.suggest.Items())
	if n > maxSuggestRows {
		n = maxSuggestRows
	}
	return n
}

// refreshSuggestions recomputes the picker from the current input text. It is
// called once per Update after the keystroke has landed in the text input, so
// typing, backspace, ctrl+u and paste all flow through the same path.
func (m *model) refreshSuggestions() {
	before := m.suggestHeight()

	// A pending question turns the input into a free-text answer box, where "/"
	// is just a character the user might legitimately be typing.
	value := m.chat.input.Value()
	prefix, ok := suggestPrefix(value)
	matches := []slashCmd(nil)
	if ok && m.chat.pendingHuman() == nil {
		matches = matchCmds(prefix)
	}

	m.chat.suggestOpen = len(matches) > 0
	// suggestFilter holds the whole input value, not just the prefix: an open
	// picker's value always starts with "/", so it can never collide with the
	// empty string a closed picker resets to — including on the very first "/",
	// whose prefix is itself empty.
	if m.chat.suggestOpen && value != m.chat.suggestFilter {
		items := make([]list.Item, len(matches))
		for i, c := range matches {
			items[i] = cmdItem{c}
		}
		m.chat.suggest.SetItems(items)
		m.chat.suggest.ResetSelected() // a narrowed list always highlights its first match
	}
	m.chat.suggestFilter = value
	if !m.chat.suggestOpen {
		m.chat.suggestFilter = ""
	}

	if h := m.suggestHeight(); h != before {
		m.chat.suggest.SetSize(m.display.width, h)
		m.resizeViewport()
	}
}

// closeSuggestions dismisses the picker and hands its rows back to the viewport.
func (m *model) closeSuggestions() {
	if !m.chat.suggestOpen {
		return
	}
	m.chat.suggestOpen = false
	m.chat.suggestFilter = ""
	m.resizeViewport()
}

// selectedSuggestion returns the highlighted command, if the picker is open.
func (m *model) selectedSuggestion() (slashCmd, bool) {
	if !m.chat.suggestOpen {
		return slashCmd{}, false
	}
	it, ok := m.chat.suggest.SelectedItem().(cmdItem)
	if !ok {
		return slashCmd{}, false
	}
	return it.slashCmd, true
}

// acceptSuggestion replaces the input with the highlighted command and closes
// the picker, leaving the cursor after the trailing space for commands that
// take an argument.
func (m *model) acceptSuggestion(c slashCmd) {
	m.chat.input.SetValue(c.completion())
	m.chat.input.CursorEnd()
	m.closeSuggestions()
}

// resizeViewport re-fits the transcript after the picker opened or closed. The
// TUI runs in the alt screen at a fixed height, so those rows have to come from
// somewhere; without re-pinning a bottomed-out viewport, opening the picker
// would look like the transcript scrolled away.
func (m *model) resizeViewport() {
	if !m.chat.ready {
		return
	}
	atBottom := m.chat.viewport.AtBottom()
	m.chat.viewport.Height = m.viewportHeight()
	m.rebuildContent()
	if atBottom {
		m.chat.viewport.GotoBottom()
	}
}
