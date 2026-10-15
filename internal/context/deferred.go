package ninectx

import (
	"cmp"
	"slices"
	"strings"

	"nine/internal/llm"
)

// Tool exposition modes (adr/deferred-tool-schemas.md). Ranked advertises the
// pinned tools and the best-ranked rest with full schemas; deferred advertises a
// small resident set plus the tools the loop has loaded, and lists every other
// tool by name in a system-prompt index; hybrid is deferred plus the few indexed
// tools that rank best against the turn's request.
const (
	ExpositionRanked   = "ranked"
	ExpositionDeferred = "deferred"
	ExpositionHybrid   = "hybrid"
)

// DefaultHybridPicks is how many tools a hybrid loop picks when
// [context] hybrid_picks is unset.
const DefaultHybridPicks = 3

// Defers reports whether mode lists tools in an index rather than dropping
// them: deferred and hybrid.
func Defers(mode string) bool { return mode == ExpositionDeferred || mode == ExpositionHybrid }

// SmallCatalogTokens is the catalog size, in estimated tokens, at or below which
// a deferred loop advertises every tool and renders no index: deferring a catalog
// that already fits saves nothing and costs a load round trip (§3.1). It is the
// §7.5 gate on the interactive orchestrator's tool block.
const SmallCatalogTokens = 2500

// catalogTools are the meta-tools that only exist to find deferred tools. A
// small catalog has nothing deferred, so it advertises neither.
var catalogTools = map[string]bool{"tool_search": true, "tool_list": true}

// IndexHeading opens the tool index section of the system prompt.
const IndexHeading = "## More tools"

const indexIntro = "Call any of these by name. Load the full schema first with\n" +
	"tool_search(names=[...]) if you are unsure of the arguments."

// deferTools splits tools into the ones advertised with a schema — resident
// first, in catalog order, then promoted, in promotion order, then the picks —
// and the rest, which it returns by name and renders as the index text. The
// picks are the picks best-ranked of the others against queryVec, in rank
// order; with no query vector, or picks 0, there are none.
func deferTools(tools []ToolWithVector, resident, promoted []string, queryVec []float32, picks int) (advertised []llm.ToolDef, indexed, picked []string, index string) {
	total := 0
	for _, tw := range tools {
		if !catalogTools[tw.Tool.Name] {
			total += toolTokens(tw.Tool)
		}
	}
	if total <= SmallCatalogTokens {
		for _, tw := range tools {
			if !catalogTools[tw.Tool.Name] {
				advertised = append(advertised, tw.Tool)
			}
		}
		return advertised, nil, nil, ""
	}

	isResident := make(map[string]bool, len(resident))
	for _, n := range resident {
		isResident[n] = true
	}
	byName := make(map[string]ToolWithVector, len(tools))
	for _, tw := range tools {
		byName[tw.Tool.Name] = tw
	}
	shown := make(map[string]bool, len(resident)+len(promoted))
	for _, tw := range tools {
		if isResident[tw.Tool.Name] {
			advertised = append(advertised, tw.Tool)
			shown[tw.Tool.Name] = true
		}
	}
	for _, n := range promoted {
		if tw, ok := byName[n]; ok && !shown[n] {
			advertised = append(advertised, tw.Tool)
			shown[n] = true
		}
	}

	for _, tw := range pickTools(tools, shown, queryVec, picks) {
		advertised = append(advertised, tw.Tool)
		picked = append(picked, tw.Tool.Name)
		shown[tw.Tool.Name] = true
	}

	var rest []ToolWithVector
	for _, tw := range tools {
		if !shown[tw.Tool.Name] {
			rest = append(rest, tw)
			indexed = append(indexed, tw.Tool.Name)
		}
	}
	return advertised, indexed, picked, renderIndex(rest)
}

// pickTools returns up to k of the tools not yet shown, best-ranked against
// queryVec by cosine similarity, skipping tools with no vector and the catalog
// meta-tools. It is the ranked mode's selection, applied to what deferral
// would otherwise only index.
func pickTools(tools []ToolWithVector, shown map[string]bool, queryVec []float32, k int) []ToolWithVector {
	if k <= 0 || len(queryVec) == 0 {
		return nil
	}
	type scored struct {
		tw    ToolWithVector
		score float32
	}
	var cs []scored
	for _, tw := range tools {
		if shown[tw.Tool.Name] || catalogTools[tw.Tool.Name] || len(tw.Vector) == 0 {
			continue
		}
		cs = append(cs, scored{tw, cosineSim(queryVec, tw.Vector)})
	}
	slices.SortStableFunc(cs, func(a, b scored) int { return cmp.Compare(b.score, a.score) })
	out := make([]ToolWithVector, 0, min(k, len(cs)))
	for _, c := range cs[:min(k, len(cs))] {
		out = append(out, c.tw)
	}
	return out
}

// renderIndex lists tools by toolkit, one line per toolkit. Toolkits appear in
// the order their first tool does in the catalog, and tools in catalog order,
// so the index is identical turn to turn for a given catalog and stays in the
// reusable prefix. A tool's gloss, when it has one, follows its name.
func renderIndex(tools []ToolWithVector) string {
	if len(tools) == 0 {
		return ""
	}
	var order []string
	lines := map[string][]string{}
	for _, tw := range tools {
		kit := tw.Toolkit
		if kit == "" {
			kit = "other"
		}
		if _, ok := lines[kit]; !ok {
			order = append(order, kit)
		}
		entry := tw.Tool.Name
		if tw.Gloss != "" {
			entry += " (" + tw.Gloss + ")"
		}
		lines[kit] = append(lines[kit], entry)
	}
	var b strings.Builder
	b.WriteString(IndexHeading + "\n" + indexIntro + "\n")
	for _, kit := range order {
		b.WriteString("\n" + kit + ": " + strings.Join(lines[kit], ", "))
	}
	return b.String()
}
