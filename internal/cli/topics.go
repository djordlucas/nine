package cli

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"nine/docs"
	"nine/spec"
)

// docBundle is an embedded Markdown tree exposed as `nine <cmd> <topic>`. Both
// `nine docs` and `nine spec` are the same machinery over a different FS.
type docBundle struct {
	fsys   fs.FS
	cmd    string          // subcommand name, e.g. "docs" or "spec"
	desc   string          // one-line header shown when listing topics
	hidden map[string]bool // base names to omit from the bundle (keyed without .md)
}

func docsBundle() docBundle {
	// ROADMAP is an internal planning doc, not user-facing reference material.
	return docBundle{fsys: docs.FS, cmd: "docs", desc: "Bundled documentation", hidden: map[string]bool{"ROADMAP": true}}
}

func specBundle() docBundle {
	return docBundle{fsys: spec.FS, cmd: "spec", desc: "Bundled specification"}
}

// Docs renders a bundled documentation topic, or lists the topics when name is
// empty (`nine docs` vs `nine docs <topic>`).
func (c *CLI) Docs(name string) error { return c.renderBundle(docsBundle(), name) }

// Spec renders a bundled specification topic, or lists them when name is empty.
func (c *CLI) Spec(name string) error { return c.renderBundle(specBundle(), name) }

// docTopic is one addressable Markdown document within a bundle.
type docTopic struct {
	name  string // command-line address: base filename without .md
	path  string // path within the embedded FS
	title string // first Markdown heading, shown in listings
}

// renderBundle prints one topic's rendered Markdown, or the topic list when name
// is empty. An unknown name is an error that points back at the listing.
func (c *CLI) renderBundle(b docBundle, name string) error {
	if name == "" {
		return c.listTopics(b)
	}
	p, ok := b.resolve(name)
	if !ok {
		return fmt.Errorf("unknown %s topic %q; run `nine %s` to list topics", b.cmd, name, b.cmd)
	}
	data, err := fs.ReadFile(b.fsys, p)
	if err != nil {
		return fmt.Errorf("read %s: %w", p, err)
	}
	return c.writeMarkdown(string(data))
}

// listTopics writes the available topics with their titles, aligned in columns.
// The listing is plain text (not Markdown) so it stays readable when piped.
func (c *CLI) listTopics(b docBundle) error {
	topics, err := b.topics()
	if err != nil {
		return err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — run `nine %s <topic>`\n\n", b.desc, b.cmd)
	width := 0
	for _, t := range topics {
		if len(t.name) > width {
			width = len(t.name)
		}
	}
	for _, t := range topics {
		if t.title != "" {
			fmt.Fprintf(&sb, "  %-*s  %s\n", width, t.name, t.title)
		} else {
			fmt.Fprintf(&sb, "  %s\n", t.name)
		}
	}
	_, err = io.WriteString(c.Out, sb.String())
	return err
}

// topics returns every Markdown document in the bundle, sorted by name. A base
// name that collides across subdirectories keeps the shallower entry under its
// short name; the deeper one is addressed by its full relative path.
func (b docBundle) topics() ([]docTopic, error) {
	var topics []docTopic
	seen := map[string]bool{}
	err := fs.WalkDir(b.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		name := strings.TrimSuffix(path.Base(p), ".md")
		if b.hidden[name] {
			return nil
		}
		if seen[name] {
			name = strings.TrimSuffix(p, ".md")
		}
		seen[name] = true
		topics = append(topics, docTopic{name: name, path: p, title: firstHeading(b.fsys, p)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].name < topics[j].name })
	return topics, nil
}

// resolve maps a topic name to a path within the bundle. It accepts the short
// name (base filename) or an explicit relative path such as
// "contracts/event-journal", with or without the .md suffix.
func (b docBundle) resolve(name string) (string, bool) {
	topics, err := b.topics()
	if err != nil {
		return "", false
	}
	for _, t := range topics {
		if t.name == name {
			return t.path, true
		}
	}
	cand := name
	if !strings.HasSuffix(cand, ".md") {
		cand += ".md"
	}
	if b.hidden[strings.TrimSuffix(path.Base(cand), ".md")] {
		return "", false
	}
	if info, err := fs.Stat(b.fsys, cand); err == nil && !info.IsDir() {
		return cand, true
	}
	return "", false
}

// firstHeading returns the text of the first Markdown ATX heading in the file,
// used as a human-readable title in topic listings.
func firstHeading(fsys fs.FS, p string) string {
	f, err := fsys.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "# "))
		}
	}
	return ""
}
