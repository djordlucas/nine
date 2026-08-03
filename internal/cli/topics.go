package cli

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"nine/internal/docindex"
)

// Docs renders a bundled documentation topic, or lists the topics when name is
// empty (`nine docs` vs `nine docs <topic>`). Addressing lives in docindex, so
// the CLI and the agent's doc tools resolve a topic name identically.
func (c *CLI) Docs(name string) error { return c.renderBundle(docindex.Docs(), name) }

// Spec renders a bundled specification topic, or lists them when name is empty.
func (c *CLI) Spec(name string) error { return c.renderBundle(docindex.Spec(), name) }

// renderBundle prints one topic's rendered Markdown, or the topic list when name
// is empty. An unknown name is an error that points back at the listing.
func (c *CLI) renderBundle(b docindex.Bundle, name string) error {
	if name == "" {
		return c.listTopics(b)
	}
	p, ok := b.Resolve(name)
	if !ok {
		return fmt.Errorf("unknown %s topic %q; run `nine %s` to list topics", b.Cmd, name, b.Cmd)
	}
	data, err := fs.ReadFile(b.FS, p)
	if err != nil {
		return fmt.Errorf("read %s: %w", p, err)
	}
	return c.writeMarkdown(string(data))
}

// listTopics writes the available topics with their titles, aligned in columns.
// The listing is plain text (not Markdown) so it stays readable when piped.
func (c *CLI) listTopics(b docindex.Bundle) error {
	topics, err := b.Topics()
	if err != nil {
		return err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — run `nine %s <topic>`\n\n", b.Desc, b.Cmd)
	width := 0
	for _, t := range topics {
		if len(t.Name) > width {
			width = len(t.Name)
		}
	}
	for _, t := range topics {
		if t.Title != "" {
			fmt.Fprintf(&sb, "  %-*s  %s\n", width, t.Name, t.Title)
		} else {
			fmt.Fprintf(&sb, "  %s\n", t.Name)
		}
	}
	_, err = io.WriteString(c.Out, sb.String())
	return err
}
