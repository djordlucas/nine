package ninectx

import (
	"strings"

	"nine/internal/llm"
)

// Tool exposition modes (adr/deferred-tool-schemas.md). Ranked advertises the
// pinned tools and the best-ranked rest with full schemas; deferred advertises a
// small resident set plus the tools the loop has loaded, and lists every other
// tool by name in a system-prompt index.
const (
	ExpositionRanked   = "ranked"
	ExpositionDeferred = "deferred"
)

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
// first, in catalog order, then promoted, in promotion order — and the rest,
// which it returns by name and renders as the index text.
func deferTools(tools []ToolWithVector, resident, promoted []string) (advertised []llm.ToolDef, indexed []string, index string) {
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
		return advertised, nil, ""
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

	var rest []ToolWithVector
	for _, tw := range tools {
		if !shown[tw.Tool.Name] {
			rest = append(rest, tw)
			indexed = append(indexed, tw.Tool.Name)
		}
	}
	return advertised, indexed, renderIndex(rest)
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
