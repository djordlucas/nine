# TUI Boxed Messages

**Status:** proposed  **Scope:** `internal/tui` only  no wire-protocol,
daemon, or persistence changes.

> This records the design decision to render chat messages in visual boxes
> using lipgloss border styles, addressing the rightmost column display issue
> in the current TUI implementation.

## Problem Statement

The current TUI message rendering has a visual display issue: **the rightmost
column is never properly displayed** when attempting to render messages in
boxes. This stems from the current approach using manual indentation instead
of proper border rendering.

### Current Implementation Issues

1. **Manual Indentation Only**: The `renderChatMsg` function uses `const indent = "  "`
   for visual separation, not actual box borders. This means there's no visual
   container around messages.

2. **Width Miscalculations**: Text wrapping uses `wordWrap(msg.text, width-len(indent))`
   but doesn't account for border characters (2 chars: left + right border).
   When the viewport renders, it truncates at its exact width without considering
   that visual borders would consume additional space.

3. **Timestamp Placement**: The current `headerLine` function places the timestamp
   on the same line as the role label (`You:`, `Nine:`), which doesn't work well
   with boxed content where the timestamp should be part of the box.

4. **No Visual Hierarchy**: Without proper borders, it's harder to visually
   distinguish between different message types and their boundaries, especially
   in long conversations.

## Decision

**Use lipgloss border styles to render messages in proper boxes** instead of
manual indentation. This provides:

- Correct width calculations that account for border characters
- Visual distinction between message types
- Consistent spacing and alignment
- Better readability in long conversations

## Proposed Design

### Border Style Assignments

| Message Type | Border Style | Border Color | Rationale |
|--------------|--------------|--------------|-----------|
| User | `RoundedBorder()` | 127 (purple) | Friendly, approachable appearance for user input |
| Nine | `NormalBorder()` | 86 (green) | Clean, standard look for assistant responses |
| System | `HiddenBorder()` | N/A | Subtle separation for system messages (no visual border) |
| Ask/HITL | `DoubleBorder()` | 214 (orange) | High visibility for questions requiring user action |

### Palette Extension

Add box-specific styles to the existing `palette` struct in `internal/tui/tui.go`:

```go
type palette struct {
    // ... existing fields ...
    
    // Box styles for message rendering
    userBox   lipgloss.Style
    nineBox   lipgloss.Style  
    systemBox lipgloss.Style
    askBox    lipgloss.Style
}
```

Each style will be configured with:
- `BorderStyle()`: The appropriate border type
- `BorderForeground()`: The color matching the role's theme
- `Padding()`: Internal padding (0, 1 for horizontal padding)
- `Margin()`: External margin for spacing between messages

### Width Calculation

The key fix for the rightmost column issue:

```go
// Available width for the box content
contentWidth := width - lipgloss.Width(timestamp) - 1

// Text must be wrapped to fit INSIDE the box (accounting for borders)
textWidth := contentWidth - 2  // -2 for left and right border characters

// Then wrap and render
wrapped := wordWrap(msg.text, textWidth)
box := boxStyle.Width(contentWidth).Render(timestamp + " " + roleLabel + "\n" + wrapped)
```

This ensures that:
1. The box itself fits within the viewport width
2. The text inside the box doesn't overflow the box boundaries
3. The rightmost character of the text is visible

### Message Layout

Each message will have this structure:

```
┌─────────────────────────────────────────────────────────┐
│[15:04:05] You:                                         [ts]│
│  This is the user message text that wraps properly        │
│  within the box boundaries.                                  │
└─────────────────────────────────────────────────────────┘
```

Note: The timestamp moves **inside** the box as part of the header line, which
provides better visual hierarchy and solves the alignment issues.

## Implementation Plan

### Phase 1: Palette Extension (Low Risk)

**Files:** `internal/tui/tui.go`

- Add `userBox`, `nineBox`, `systemBox`, `askBox` fields to `palette` struct
- Initialize these styles in `autoPalette()`, `lightPalette()`, `darkPalette()`
- No changes to rendering logic yet

**Testing:** Verify existing functionality unchanged (no visual regression)

### Phase 2: Update `renderChatMsg` (Medium Risk)

**Files:** `internal/tui/tui.go`

- Modify `renderChatMsg` to use box styles instead of manual indentation
- Fix width calculations to account for borders
- Move timestamp inside the box
- Ensure markdown rendering respects box width

**Key changes:**
```go
func renderChatMsg(sb *strings.Builder, msg chatMsg, width int, showDetail bool, pal palette, r *glamour.TermRenderer) {
    tsText := "[" + msg.at.Format("15:04:05") + "]"
    
    var boxStyle lipgloss.Style
    var roleLabel string
    
    switch msg.role {
    case "user":
        boxStyle = pal.userBox
        roleLabel = pal.you.Render("You:")
    case "nine":
        boxStyle = pal.nineBox
        roleLabel = pal.nine.Render("Nine:")
    case "system":
        boxStyle = pal.systemBox
        roleLabel = pal.system.Render("nine:")
    case "ask":
        boxStyle = pal.askBox
        roleLabel = pal.tool.Render("Nine asks:")
    }
    
    // Calculate available width
    tsStyled := pal.ts.Render(tsText)
    contentWidth := width - lipgloss.Width(tsStyled) - 1
    textWidth := contentWidth - 2  // Account for borders
    
    // Build content
    header := tsStyled + " " + roleLabel
    var content strings.Builder
    content.WriteString(header)
    
    // Add message text (properly wrapped)
    if msg.text != "" {
        if content.Len() > 0 {
            content.WriteString("\n")
        }
        wrapped := wordWrap(msg.text, textWidth)
        content.WriteString(wrapped)
    }
    
    // Render the box
    box := boxStyle.Width(contentWidth).Render(content.String())
    sb.WriteString(box + "\n")
}
```

**Testing:** 
- Test with various terminal widths (80, 120, 160+ columns)
- Verify all message types render correctly
- Check that long messages wrap properly
- Verify light and dark themes

### Phase 3: Update Tool Event Rendering (Medium Risk)

**Files:** `internal/tui/tui.go`

- Modify `renderToolEvent` to work within boxed message context
- Adjust width calculations for tool input/output
- Ensure tool events are properly indented within their parent message box

**Key consideration:** Tool events should be rendered as part of the Nine message
box, with appropriate internal indentation:

```
┌─────────────────────────────────────────────────────────┐
│[15:04:05] Nine:                                        [ts]│
│  Response text here...                                  │
│  ┌─ tool_name (1.2s)                                    │
│  │ input: tool argument here                           │
│  └─ output: tool result here                            │
└─────────────────────────────────────────────────────────┘
```

Or, simpler approach: keep tool events as indented text within the Nine box,
without nested boxes (to avoid visual clutter):

```
┌─────────────────────────────────────────────────────────┐
│[15:04:05] Nine:                                        [ts]│
│  Response text here...                                  │
│  [3m→[0m tool_name 1.2s                                 [ts]│
│    input: tool argument here                           │
│    [3m↓[0m output: tool result here                          │
└─────────────────────────────────────────────────────────┘
```

**Testing:**
- Verify tool events render correctly within boxed messages
- Check that tool input/output truncation still works
- Test with detail view on/off

### Phase 4: Update Thinking Rendering (Medium Risk)

**Files:** `internal/tui/tui.go`

- Modify `renderThinking` to use boxed layout
- Ensure live streaming text fits within box
- Handle spinner placement within box

### Phase 5: Final Validation (Low Risk)

- Comprehensive testing across all scenarios
- Performance check (rendering should not slow down significantly)
- Memory usage check (no significant increase)

## Migration Strategy

### Backward Compatibility

This change is **fully backward compatible** from a user perspective:
- No changes to command-line interface
- No changes to configuration
- No changes to wire protocol
- Only visual presentation changes

### Feature Flag (Optional)

If desired, a feature flag could be added to toggle between boxed and
non-boxed rendering:

```go
// In config.Config
type Config struct {
    // ... existing fields ...
    TUIBoxedMessages bool `toml:"tui_boxed_messages"`
}
```

However, given that this fixes a display bug (rightmost column cutoff) rather
than just adding a feature, **immediate full adoption is recommended**.

## Rejected Alternatives

### Alternative 1: Use `bubbles/box` Component

The Bubble Tea ecosystem includes a `github.com/charmbracelet/bubbles/box`
component. However:

- **Rejected** because lipgloss styles are simpler and more lightweight
- lipgloss is already a dependency and provides all needed functionality
- `bubbles/box` would add unnecessary complexity and another dependency
- lipgloss borders are more flexible (can combine with other styles)

### Alternative 2: Custom Border Drawing

Manually drawing borders with Unicode box-drawing characters:

- **Rejected** because lipgloss already handles this correctly
- Would duplicate functionality that already exists
- lipgloss handles terminal width, color, and edge cases properly
- More code to maintain

### Alternative 3: ASCII-Only Borders

Using ASCII characters like `|`, `-`, `+` for borders:

- **Rejected** because Unicode box-drawing characters are standard
- Modern terminals support Unicode well
- ASCII borders look less professional
- lipgloss already uses Unicode borders by default

### Alternative 4: No Boxes, Just Better Indentation

Improving the manual indentation approach:

- **Rejected** because it doesn't solve the fundamental problem
- The rightmost column issue is inherent to manual indentation
- Boxes provide better visual hierarchy and readability
- Users expect modern chat UIs to have message bubbles/boxes

## Testing Strategy

### Unit Tests

- Add tests for new box style configurations
- Test width calculations with various inputs
- Test message wrapping within boxes

### Integration Tests

- Test full TUI rendering with boxed messages
- Verify all message types work correctly
- Test with different terminal widths

### Manual Testing

- Visual inspection of message rendering
- Test with light and dark terminal themes
- Test with different terminal sizes
- Verify accessibility (color contrast, etc.)

### Performance Testing

- Ensure rendering performance doesn't degrade
- Check memory usage with long conversations
- Verify no rendering artifacts or flickering

## Open Questions

1. **Nested boxes for tool events**: Should tool events have their own boxes
   within the Nine message box, or use simple indentation?
   - **Recommendation**: Simple indentation (no nested boxes) to avoid visual
     clutter

2. **Box padding**: How much padding inside boxes?
   - **Recommendation**: 1 space horizontal padding, 0 vertical (single line
     messages don't need vertical padding)

3. **Margin between messages**: How much space between consecutive message boxes?
   - **Recommendation**: 1 line margin for readability

4. **Markdown in boxes**: Should markdown-rendered content respect box width?
   - **Recommendation**: Yes, markdown should wrap to box width

5. **Very narrow terminals**: What happens when terminal is too narrow for boxes?
   - **Recommendation**: Fall back to non-boxed rendering when width < 40

## Success Criteria

- [ ] Rightmost column is properly displayed (no truncation)
- [ ] All message types render correctly in boxes
- [ ] Visual hierarchy is improved
- [ ] No performance regression
- [ ] Works with light and dark themes
- [ ] Works with various terminal widths
- [ ] Backward compatible (no breaking changes)

## References

- Current implementation: `internal/tui/tui.go` lines ~800-1200
- lipgloss documentation: https://github.com/charmbracelet/lipgloss
- Bubble Tea: https://github.com/charmbracelet/bubbletea
- Related: `adr/tui-slash-suggestions.md` (shows pattern for TUI improvements)
