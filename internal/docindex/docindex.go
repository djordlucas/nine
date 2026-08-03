// Package docindex addresses the Markdown bundles embedded in the binary — the
// documentation (docs/) and the specification (spec/) — both as whole topics
// and as section-level chunks.
//
// It is the single source of truth for how a bundled document is named and
// addressed. The CLI (`nine docs <topic>`) and the agent's doc_search/doc_read
// tools both resolve through here, so an address Nine reports in an answer is
// one a human can paste straight into the CLI.
package docindex

import (
	"bufio"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"nine/docs"
	"nine/spec"
)

// SectionMaxBytes is the size above which an H2 section is split further at its
// H3 headings. Oversized chunks retrieve badly in both directions: one vector
// averaged over several unrelated subtopics ranks poorly for all of them, and a
// hit reads back as far more text than the caller asked for.
const SectionMaxBytes = 6000

// Bundle is an embedded Markdown tree exposed as `nine <cmd> <topic>`. Both
// `nine docs` and `nine spec` are the same machinery over a different FS.
type Bundle struct {
	FS     fs.FS
	Cmd    string          // subcommand and address prefix, e.g. "docs" or "spec"
	Desc   string          // one-line header shown when listing topics
	Hidden map[string]bool // base names to omit from the bundle (keyed without .md)
}

// Docs is the user-facing documentation bundle.
func Docs() Bundle {
	// ROADMAP is an internal planning doc, not user-facing reference material.
	return Bundle{FS: docs.FS, Cmd: "docs", Desc: "Bundled documentation", Hidden: map[string]bool{"ROADMAP": true}}
}

// Spec is the project specification bundle.
func Spec() Bundle {
	return Bundle{FS: spec.FS, Cmd: "spec", Desc: "Bundled specification"}
}

// Bundles returns every addressable bundle, in the order they are searched when
// a reference does not name one.
func Bundles() []Bundle { return []Bundle{Docs(), Spec()} }

// Topic is one addressable Markdown document within a bundle.
type Topic struct {
	Name  string // command-line address: base filename without .md
	Path  string // path within the embedded FS
	Title string // first Markdown heading, shown in listings
}

// Topics returns every Markdown document in the bundle, sorted by name. A base
// name that collides across subdirectories keeps the shallower entry under its
// short name; the deeper one is addressed by its full relative path.
func (b Bundle) Topics() ([]Topic, error) {
	var topics []Topic
	seen := map[string]bool{}
	err := fs.WalkDir(b.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil
		}
		name := strings.TrimSuffix(path.Base(p), ".md")
		if b.Hidden[name] {
			return nil
		}
		if seen[name] {
			name = strings.TrimSuffix(p, ".md")
		}
		seen[name] = true
		topics = append(topics, Topic{Name: name, Path: p, Title: firstHeading(b.FS, p)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(topics, func(i, j int) bool { return topics[i].Name < topics[j].Name })
	return topics, nil
}

// Resolve maps a topic name to a path within the bundle. It accepts the short
// name (base filename) or an explicit relative path such as
// "contracts/event-journal", with or without the .md suffix.
func (b Bundle) Resolve(name string) (string, bool) {
	topics, err := b.Topics()
	if err != nil {
		return "", false
	}
	for _, t := range topics {
		if t.Name == name {
			return t.Path, true
		}
	}
	cand := name
	if !strings.HasSuffix(cand, ".md") {
		cand += ".md"
	}
	if b.Hidden[strings.TrimSuffix(path.Base(cand), ".md")] {
		return "", false
	}
	if info, err := fs.Stat(b.FS, cand); err == nil && !info.IsDir() {
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

// Section is one addressable slice of a bundled document: the unit that gets
// embedded, ranked, and read back. Addr is its stable identity — the bundle
// path plus a heading anchor, e.g. "docs/skills.md#tools".
type Section struct {
	Addr    string `json:"addr"`
	Bundle  string `json:"bundle"`
	Path    string `json:"path"`  // path within the bundle FS
	Topic   string `json:"topic"` // short CLI-addressable name
	Title   string `json:"title"` // document title (first heading)
	Heading string `json:"heading,omitempty"`
	Body    string `json:"body"`
}

// EmbedText is the text indexed for s. The document title and section heading
// are prepended because a section rarely repeats its own context: the "Tools"
// section of skills.md never says "skill", so indexing the body alone makes it
// unfindable by the words someone would actually ask with.
func (s Section) EmbedText() string {
	var b strings.Builder
	b.WriteString(s.Title)
	if s.Heading != "" {
		b.WriteString(" — ")
		b.WriteString(s.Heading)
	}
	b.WriteString("\n")
	b.WriteString(s.Body)
	return b.String()
}

// Sections splits every document in every bundle into addressable chunks. It is
// what the boot-time indexer walks.
func Sections() ([]Section, error) {
	var all []Section
	for _, b := range Bundles() {
		topics, err := b.Topics()
		if err != nil {
			return nil, err
		}
		for _, t := range topics {
			secs, err := b.Sections(t)
			if err != nil {
				return nil, err
			}
			all = append(all, secs...)
		}
	}
	return all, nil
}

// Sections splits one document into chunks at its H2 headings, further
// splitting any oversized section at its H3s. Text before the first H2 (the
// title and any preamble) becomes a leading section with no heading.
func (b Bundle) Sections(t Topic) ([]Section, error) {
	data, err := fs.ReadFile(b.FS, t.Path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", t.Path, err)
	}
	lines := strings.Split(string(data), "\n")

	base := Section{Bundle: b.Cmd, Path: t.Path, Topic: t.Name, Title: t.Title}
	var out []Section
	used := map[string]bool{}
	for _, blk := range splitHeadings(lines, 2) {
		// An oversized H2 is re-split at its H3s; its own preamble stays under
		// the H2 anchor so the section is never silently dropped.
		blocks := []block{blk}
		if len(blk.body) > SectionMaxBytes {
			blocks = splitHeadings(blk.lines, 3)
			for i := range blocks {
				if blocks[i].heading == "" {
					blocks[i].heading = blk.heading
				}
			}
		}
		for _, sub := range blocks {
			if strings.TrimSpace(sub.body) == "" {
				continue
			}
			s := base
			s.Heading = sub.heading
			s.Body = strings.TrimSpace(sub.body)
			// The leading section has no H2 of its own, but it still needs an
			// anchor: an unanchored address is the address of the *whole*
			// document, so leaving it bare would make a hit on the intro read
			// back as the entire file. Fall back to the document title, which
			// is the H1 that section actually opens with.
			head := sub.heading
			if head == "" {
				head = t.Title
			}
			if head == "" {
				head = "intro"
			}
			s.Addr = b.Cmd + "/" + t.Path + anchor(head, used)
			out = append(out, s)
		}
	}
	return out, nil
}

// block is a heading and the lines beneath it, before addressing.
type block struct {
	heading string
	lines   []string
	body    string
}

// splitHeadings cuts lines at ATX headings of exactly the given level, ignoring
// headings inside fenced code blocks — the bundled docs quote Markdown samples
// (skills.md carries a whole skill file, headings and all), and splitting on
// those would shred a document into fragments that address nothing.
func splitHeadings(lines []string, level int) []block {
	prefix := strings.Repeat("#", level) + " "
	var blocks []block
	cur := block{}
	fenced := false
	for _, line := range lines {
		if isFence(line) {
			fenced = !fenced
		}
		if !fenced && strings.HasPrefix(line, prefix) {
			blocks = append(blocks, cur)
			cur = block{heading: strings.TrimSpace(strings.TrimPrefix(line, prefix))}
		}
		cur.lines = append(cur.lines, line)
	}
	blocks = append(blocks, cur)
	for i := range blocks {
		blocks[i].body = strings.Join(blocks[i].lines, "\n")
	}
	return blocks
}

func isFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

// anchor renders a heading as a URL-style fragment, disambiguating repeats
// within one document so every address resolves to exactly one section.
func anchor(heading string, used map[string]bool) string {
	if heading == "" {
		return ""
	}
	s := slug(heading)
	if s == "" {
		return ""
	}
	cand := s
	for n := 2; used[cand]; n++ {
		cand = fmt.Sprintf("%s-%d", s, n)
	}
	used[cand] = true
	return "#" + cand
}

func slug(h string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Fetch resolves a reference and returns the text it names. ref accepts every
// address a caller might plausibly hold: a short topic name ("agent-loop"), a
// bundle-qualified path ("docs/agent-loop.md"), a path inside a bundle
// ("contracts/event-journal"), any of those with or without .md, and any of
// those with a "#section" anchor. Without an anchor the whole document comes
// back with an empty Heading.
func Fetch(ref string) (Section, bool, error) {
	ref = strings.TrimSpace(ref)
	docRef, frag, hasFrag := strings.Cut(ref, "#")
	b, p, ok := resolveRef(docRef)
	if !ok {
		return Section{}, false, nil
	}
	title := firstHeading(b.FS, p)
	topic := strings.TrimSuffix(path.Base(p), ".md")
	if !hasFrag {
		data, err := fs.ReadFile(b.FS, p)
		if err != nil {
			return Section{}, false, fmt.Errorf("read %s: %w", p, err)
		}
		return Section{
			Addr: b.Cmd + "/" + p, Bundle: b.Cmd, Path: p, Topic: topic,
			Title: title, Body: strings.TrimSpace(string(data)),
		}, true, nil
	}
	secs, err := b.Sections(Topic{Name: topic, Path: p, Title: title})
	if err != nil {
		return Section{}, false, err
	}
	want := b.Cmd + "/" + p + "#" + frag
	for _, s := range secs {
		if s.Addr == want {
			return s, true, nil
		}
	}
	return Section{}, false, nil
}

// resolveRef maps a document reference to its bundle and path. A reference may
// name its bundle up front ("spec/overview"); otherwise every bundle is tried
// in order, so short names keep working the way they do on the CLI.
func resolveRef(ref string) (Bundle, string, bool) {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "./")
	for _, b := range Bundles() {
		if rest, ok := strings.CutPrefix(ref, b.Cmd+"/"); ok {
			if p, found := b.Resolve(rest); found {
				return b, p, true
			}
		}
	}
	for _, b := range Bundles() {
		if p, found := b.Resolve(ref); found {
			return b, p, true
		}
	}
	return Bundle{}, "", false
}

// TopicLines renders every addressable topic as "bundle/name — Title" lines. It
// backs the "unknown reference" error, which is the agent's enumeration path:
// a wrong guess answers itself with the full catalog instead of costing a
// second tool call.
func TopicLines() []string {
	var out []string
	for _, b := range Bundles() {
		topics, err := b.Topics()
		if err != nil {
			continue
		}
		for _, t := range topics {
			line := b.Cmd + "/" + t.Name
			if t.Title != "" {
				line += " — " + t.Title
			}
			out = append(out, line)
		}
	}
	return out
}
