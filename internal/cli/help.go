package cli

import (
	"io"
	"os"

	"github.com/charmbracelet/glamour"
	"golang.org/x/term"

	"nine/docs"
)

// Help renders the CLI usage reference. The source is docs/usage.md, embedded at
// build time (see package docs), so the help text can never drift from the docs.
func (c *CLI) Help() error {
	return c.writeMarkdown(docs.Usage)
}

// writeMarkdown writes md to Out. When Out is a terminal the Markdown is
// rendered with ANSI styling; otherwise it is written verbatim, so piping or
// redirecting yields clean, greppable text.
func (c *CLI) writeMarkdown(md string) error {
	if f, ok := c.Out.(*os.File); ok && term.IsTerminal(int(f.Fd())) { //nolint:gosec // G115: a file descriptor always fits in int
		if rendered, err := renderMarkdown(md, terminalWidth(f)); err == nil {
			_, err = io.WriteString(c.Out, rendered)
			return err
		}
		// Fall through to raw output if rendering fails for any reason.
	}
	_, err := io.WriteString(c.Out, md)
	return err
}

// renderMarkdown styles Markdown for the terminal, auto-selecting a light/dark
// theme and wrapping to the given width.
func renderMarkdown(md string, width int) (string, error) {
	r, err := glamour.NewTermRenderer(glamour.WithAutoStyle(), glamour.WithWordWrap(width))
	if err != nil {
		return "", err
	}
	return r.Render(md)
}

// terminalWidth returns a sensible wrap width for f, capped at 100 columns so
// long lines stay readable on very wide terminals. It falls back to 80 when the
// size can't be determined.
func terminalWidth(f *os.File) int {
	w, _, err := term.GetSize(int(f.Fd())) //nolint:gosec // G115: a file descriptor always fits in int
	if err != nil || w <= 0 {
		return 80
	}
	if w > 100 {
		return 100
	}
	return w
}
