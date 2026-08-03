# TUI slash-command suggestions

**Status:** implemented · **Scope:** `internal/tui` only — no wire-protocol,
daemon, or persistence changes.

> This records the design and the reasoning behind it. The behaviour itself is
> documented in [usage.md](usage.md#tui-slash-commands) and pinned by
> `spec/conformance.md` §10.

## Goal

When the user types `/` at the start of the TUI input box, a picker opens above
the input listing the available slash commands — name on the left, a short
description on the right. It filters as the user keeps typing, and it closes as
soon as the leading `/` is gone (or nothing matches). Built on the `bubbles`
list fragment, driven by the existing bubbletea `model`.

## Why the current code needs a catalog first

The set of slash commands is spelled out in **three** places today, and they
already disagree:

| Where | Contents |
|---|---|
| `runCmd`'s switch (`internal/tui/commands.go:18`) | 11 commands |
| `cmdHelp()`'s hardcoded string (`commands.go:46`) | 14 entries |
| the `tea.KeyEnter` branch (`internal/tui/tui.go:381`) | `/think`, `/clear`, `/new` handled inline |

A picker is a fourth copy unless the list becomes data. Step 1 fixes that; every
later step reads from the one catalog.

---

## 1 · One catalog, in `commands.go`

```go
// slashCmd is one entry in the TUI's command catalog: the single source of
// truth behind /help, the suggestion picker, and the executor coverage test.
type slashCmd struct {
    name string // without the leading slash
    args string // "" | "[filter]" | "<mode>" — rendered after the name
    desc string // one short line, shown on the right in the picker
}

var slashCmds = []slashCmd{ /* most-used first; includes new, clear, think */ }
```

Order is curated rather than alphabetical: it is what `/help` prints *and* what
the picker shows before typing narrows it, and only `maxSuggestRows` fit on
screen, so the commands worth reaching for should be the ones visible first.

- `cmdHelp()` is **generated** from `slashCmds` — the hardcoded block goes away,
  so help and the picker can never drift. The generated output is byte-identical
  to the string it replaced.
- `matchCmds(prefix string) []slashCmd` — case-insensitive prefix match on the
  name. Pure function, no bubbletea, directly unit-testable.
- Descriptions are written short deliberately (≈40 cols) so the picker's right
  column fits an 80-column terminal without truncation.

## 2 · The fragment — new file `internal/tui/suggest.go`

Uses `github.com/charmbracelet/bubbles/list`. The repository vendors its
dependencies, and `list` is not a leaf: taking it added `bubbles/help`,
`bubbles/paginator` and `github.com/sahilm/fuzzy` (MIT, ~400 LoC) to `vendor/`.
`fuzzy` backs the list's own filtering, which this picker switches off — that is
the price of using the stock fragment instead of hand-rolling one.

- `cmdItem` wraps `slashCmd` and implements `list.Item` (`FilterValue` returns
  `"/" + name`).
- `cmdDelegate` implements `list.ItemDelegate`: `Height() 1`, `Spacing() 0`,
  no-op `Update`, and a `Render` that writes one row — `/name` padded to the
  width of the longest name in the catalog (so descriptions line up in a
  column), then the description in `pal.continuation`. The selected row gets a
  `›` marker and `pal.tool` styling.
- `newSuggestList(pal)` builds it and turns **off** everything the popup does not
  want: `SetShowTitle`, `SetShowStatusBar`, `SetShowHelp`, `SetShowPagination`,
  and critically `SetFilteringEnabled(false)` — the list's own filter is a modal
  prompt bound to `/`, the exact key that opens our picker. We filter by
  rebuilding `SetItems` instead. `DisableQuitKeybindings()` so it never swallows
  `ctrl+c`.
- Visible rows clamp to `maxSuggestRows = 8`.

**Rejected alternative:** `textinput`'s built-in `ShowSuggestions` /
`SetSuggestions`. It renders a single inline ghost-text completion with no
descriptions and no list — it cannot express the name/description layout asked
for.

## 3 · Model state

Added to `chatState`:

```go
suggest     list.Model
suggestOpen bool
```

One helper drives the whole lifecycle:

```go
// refreshSuggestions recomputes the picker from the current input text.
func (m *model) refreshSuggestions()
```

Open **iff all** hold:

1. no pending HITL question (`m.chat.pendingHuman() == nil`) — while a question
   is on screen the input is a free-text answer box, and `/` is just a character;
2. the input value starts with `/`;
3. it contains no space yet — once an argument is being typed (`/tools ht`) the
   picker gets out of the way;
4. `matchCmds` returned at least one entry.

Otherwise `suggestOpen = false`. Condition 2 *is* the "user backspaces the `/`,
picker closes" requirement; condition 4 covers `/zzz`. `ResetSelected()` fires
whenever the filter text changed, so the highlight always lands on the first
match rather than a stale index.

`suggestFilter` remembers the whole input value, not just the prefix. An open
picker's value always starts with `/`, so it can never collide with the empty
string a closed picker resets to — including on the very first `/`, whose prefix
is itself `""`. Storing the prefix alone made that first keystroke a no-op and
opened the picker with zero rows.

`refreshSuggestions` is called once at the bottom of `Update`, after
`m.chat.input.Update(msg)` has applied the keystroke — so typing, backspace,
paste, and `ctrl+u` all behave identically without enumerating key types.

## 4 · Key handling

A guarded block at the top of the `tea.KeyMsg` switch, active only while
`suggestOpen`, returning early so these keys never reach the textinput or
viewport:

| Key | Behaviour |
|---|---|
| `↑` / `↓` (and `ctrl+p` / `ctrl+n`) | `CursorUp` / `CursorDown`. `PgUp`/`PgDn`/`Home`/`End` keep scrolling the transcript. |
| `Tab` | Complete to `/<name>` — plus a trailing space when the command takes arguments — and close. |
| `Enter` | Accept the highlighted command. No-arg commands (`/help`, `/status`, `/new`, …) complete **and submit** in one keystroke; arg-taking ones (`/tools`, `/plan-mode`, `/think`) complete and leave the cursor after the space so the argument can be typed. |
| `Esc` | Close the picker only. |

Two of these deserve calling out:

- **`Enter`** needs an explicit rule. Without one, typing `/hel` and pressing
  Enter submits the literal text `/hel` while `/help` is visibly highlighted —
  the picker would look decorative and lie about what is selected.
- **`Esc`** is the one existing binding whose meaning changes: today it quits the
  TUI (`tui.go:326`). After this change it quits *unless* the picker is open, in
  which case it dismisses the picker. This matches every other picker users
  encounter, but it is a real behaviour change worth flagging in the PR.

## 5 · Rendering and layout

The picker sits between the transcript's lower rule and the input line, so it
reads as attached to the input box:

```
header
rule
viewport
rule
[picker]      ← new, height = visible rows, 0 when closed
input
```

The TUI runs in the alt screen at a fixed terminal height, so the viewport must
give back exactly the rows the picker takes:

```go
func (m *model) suggestHeight() int  // 0 when closed, else len(visible items)
func (m *model) viewportHeight() int // … - m.suggestHeight()
```

`refreshSuggestions` therefore also resizes the viewport whenever the picker's
height changes — same path `WindowSizeMsg` uses — and re-pins the scroll to the
bottom if it was already there, so opening the picker does not appear to scroll
the transcript.

## 6 · Tests — new `internal/tui/suggest_test.go`

All pure Go, no TTY, matching the style of `hitl_queue_test.go`:

- `matchCmds` filtering: `/` → all, `/t` → `tools`+`think`, `/tool` → `tools`,
  `/ZZ` → none, and case-insensitivity.
- Table test over the open/close predicate: `""`, `"hi"`, `"/"`, `"/to"`,
  `"/tools "`, `"/zz"`, and the pending-HITL case → expected `suggestOpen`.
- Completion text: `/tools` → `"/tools "`, `/help` → `"/help"`.
- **Catalog/executor coverage:** every entry in `slashCmds` is either handled by
  `runCmd` or is one of the three `tui.go`-local commands. This is the test that
  stops someone adding a suggestion for a command that answers
  `unknown command /x`. It calls `runCmd` with a nil client and recovers from
  the resulting panic — the assertion is only that the switch *dispatches* the
  name, matched via the `errUnknownCmd` sentinel, not what the handler does.
- Row layout: a rendered row contains both the name and the description; the
  description truncates rather than wrapping on a narrow terminal (a wrapped row
  would break the picker's height accounting); and selected and unselected rows
  are the same cell width. That last one caught a real bug — the `›` marker is
  one rune but four bytes, so padding unselected rows by `len()` shifted every
  name two columns right.

## 7 · Docs and spec

`docs/` and `spec/` are source of truth and are compiled into the binary, so
this behaviour change reconciles there too:

- `docs/usage.md` § *TUI Slash Commands* — a short paragraph on the picker
  (opens on `/`, filters as you type, `↑`/`↓`, `Tab`/`Enter`, `Esc`).
- `docs/glossary.md` — the *TUI slash commands* entry is already stale (missing
  `/context`, `/plan-mode`, `/sessions`, `/think`); refresh it and mention the
  picker.
- `spec/conformance.md` — one requirement line for the open/filter/close
  invariant.
- Finish with `/sync-nine`.

## Files touched

| File | Change |
|---|---|
| `internal/tui/commands.go` | add `slashCmd` catalog + `matchCmds`; generate `cmdHelp()` |
| `internal/tui/suggest.go` | **new** — list model, item, delegate, open/filter logic |
| `internal/tui/tui.go` | `chatState` fields, key handling, `View`, `viewportHeight` |
| `internal/tui/suggest_test.go` | **new** — unit tests |
| `docs/usage.md`, `docs/glossary.md`, `spec/conformance.md` | reconcile |
| `go.mod`, `go.sum`, `vendor/` | `bubbles/list` and its transitive deps |

Delivered on `feat/tui-slash-suggestions` via a PR, per the working agreement.
