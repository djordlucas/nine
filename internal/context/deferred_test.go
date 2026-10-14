package ninectx_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	ninectx "nine/internal/context"
	"nine/internal/llm"
)

// sizedTool returns a tool whose description makes it cost about t tokens.
func sizedTool(name, kit string, t int) ninectx.ToolWithVector {
	return ninectx.ToolWithVector{
		Tool:    llm.ToolDef{Name: name, Description: tokStr(t), InputSchema: json.RawMessage(`{"type":"object"}`)},
		Toolkit: kit,
	}
}

func toolNames(defs []llm.ToolDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}

// largeCatalog is bigger than SmallCatalogTokens, so a deferred loop indexes it.
func largeCatalog() []ninectx.ToolWithVector {
	return []ninectx.ToolWithVector{
		sizedTool("read_file", "workspace", 400),
		sizedTool("restore_file", "workspace", 400),
		sizedTool("trash_list", "workspace", 400),
		sizedTool("tool_search", "catalog", 100),
		sizedTool("tool_list", "catalog", 100),
		sizedTool("memory_get", "memory", 400),
		sizedTool("job_wait", "jobs", 400),
		sizedTool("fixture__mcp_echo", "mcp/fixture", 400),
		sizedTool("process_list", "processes", 400),
	}
}

func deferredBuilder(resident ...string) *ninectx.Builder {
	return ninectx.New(ninectx.Config{Budget: 100000, Exposition: ninectx.ExpositionDeferred, Resident: resident})
}

func TestDeferred_ResidentThenPromotedThenIndex(t *testing.T) {
	tools := largeCatalog()
	tools[7].Gloss = "echo text back"
	b := deferredBuilder("tool_search", "memory_get", "read_file")
	req, _, exp := b.BuildForCall(ninectx.BuildInput{
		SystemCore: "core",
		Tools:      tools,
		Promoted:   []string{"trash_list", "restore_file", "not_in_catalog"},
	})

	// Resident in catalog order, then promoted in promotion order; a promoted
	// name the catalog no longer has is skipped.
	want := []string{"read_file", "tool_search", "memory_get", "trash_list", "restore_file"}
	if got := toolNames(req.Tools); !slices.Equal(got, want) {
		t.Errorf("advertised = %v, want %v", got, want)
	}
	wantIndexed := []string{"tool_list", "job_wait", "fixture__mcp_echo", "process_list"}
	if !slices.Equal(exp.Indexed, wantIndexed) {
		t.Errorf("indexed = %v, want %v", exp.Indexed, wantIndexed)
	}
	// The index follows the core, one line per toolkit in catalog order, with
	// glosses where set.
	wantIndex := ninectx.IndexHeading + "\n" +
		"Call any of these by name. Load the full schema first with\n" +
		"tool_search(names=[...]) if you are unsure of the arguments.\n\n" +
		"catalog: tool_list\n" +
		"jobs: job_wait\n" +
		"mcp/fixture: fixture__mcp_echo (echo text back)\n" +
		"processes: process_list"
	if !strings.HasPrefix(req.System, "core\n\n"+wantIndex) {
		t.Errorf("system =\n%s\nwant prefix\n%s", req.System, "core\n\n"+wantIndex)
	}
	if exp.IndexTokens != ninectx.EstimateTokens(wantIndex) {
		t.Errorf("index tokens = %d, want %d", exp.IndexTokens, ninectx.EstimateTokens(wantIndex))
	}
}

func TestDeferred_SmallCatalogIsAdvertisedWhole(t *testing.T) {
	tools := []ninectx.ToolWithVector{
		sizedTool("read_file", "workspace", 300),
		sizedTool("tool_search", "catalog", 100),
		sizedTool("tool_list", "catalog", 100),
		sizedTool("memory_get", "memory", 300),
	}
	req, _, exp := deferredBuilder("read_file").BuildForCall(ninectx.BuildInput{SystemCore: "core", Tools: tools})
	// Nothing is deferred, so there is no index and nothing to search for.
	if got := toolNames(req.Tools); !slices.Equal(got, []string{"read_file", "memory_get"}) {
		t.Errorf("advertised = %v", got)
	}
	if exp.Indexed != nil || strings.Contains(req.System, ninectx.IndexHeading) {
		t.Errorf("a small catalog has no index: indexed=%v system=%q", exp.Indexed, req.System)
	}
}

func TestDeferred_ToolTokensCountTheIndex(t *testing.T) {
	b := deferredBuilder("read_file")
	rep := b.BuildReport(ninectx.BuildInput{SystemCore: "core", Tools: largeCatalog()})
	var tools ninectx.Section
	for _, s := range rep.Sections {
		if s.Name == "tools" {
			tools = s
		}
	}
	readFile := ninectx.ToolTokens(largeCatalog()[0].Tool)
	if tools.Tokens <= readFile {
		t.Errorf("tools section = %d tokens, want read_file (%d) plus the index", tools.Tokens, readFile)
	}
	if !strings.Contains(tools.Detail, "in the index") {
		t.Errorf("detail = %q", tools.Detail)
	}
}

func TestRanked_IgnoresPromotedAndIndexesNothing(t *testing.T) {
	b := ninectx.New(ninectx.Config{Budget: 100000})
	req, _, exp := b.BuildForCall(ninectx.BuildInput{SystemCore: "core", Tools: largeCatalog(), Promoted: []string{"job_wait"}})
	if exp.Indexed != nil || strings.Contains(req.System, ninectx.IndexHeading) {
		t.Errorf("ranked rendered an index: %v", exp.Indexed)
	}
	if len(req.Tools) != len(largeCatalog()) {
		t.Errorf("ranked advertised %d of %d", len(req.Tools), len(largeCatalog()))
	}
}
