// Package selfmodel assembles a dynamic self-description for the agent each turn.
package selfmodel

import (
	"context"
	"fmt"
	"os"
	"strings"

	"nine/internal/embed"
	"nine/internal/memory"
)

// Assembler builds a SystemSelf block from live data: loaded tool names,
// KV-stored self-knowledge, and vector-ranked relevant skills.
type Assembler struct {
	store    *memory.Store
	embedder embed.Embedder
	getTools func() []string
}

// New creates an Assembler.
//   - store is used to read KV keys and query skill vectors.
//   - embedder is used for semantic skill ranking; nil disables the skills section.
//   - getTools returns the current list of loaded tool names (called each turn).
func New(
	store *memory.Store,
	embedder embed.Embedder,
	getTools func() []string,
) *Assembler {
	return &Assembler{store: store, embedder: embedder, getTools: getTools}
}

// Build assembles and returns the self-model block. queryVec is the embedding
// of the current user message; nil disables semantic skill ranking.
func (a *Assembler) Build(_ context.Context, queryVec []float32) string {
	var sb strings.Builder

	// Environment
	tools := a.getTools()
	sb.WriteString("## Environment\n")
	if len(tools) > 0 {
		fmt.Fprintf(&sb, "Tools (%d): %s\n", len(tools), strings.Join(tools, ", "))
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		sb.WriteString("Runtime: Docker container\n")
	} else {
		sb.WriteString("Runtime: host\n")
	}

	// Self-knowledge from KV
	for _, key := range []string{"self/identity", "self/capabilities", "self/learned"} {
		val := a.store.KVGetString(key)
		if val != "" {
			sb.WriteString("\n## " + key + "\n")
			sb.WriteString(val + "\n")
		}
	}

	// Relevant skills (only when a query vector is available)
	if len(queryVec) > 0 {
		skills := a.querySkills(queryVec)
		if len(skills) > 0 {
			sb.WriteString("\n## Relevant skills\n")
			for _, s := range skills {
				sb.WriteString("- " + s + "\n")
			}
		}
	}

	return sb.String()
}

func (a *Assembler) querySkills(queryVec []float32) []string {
	results, err := a.store.VectorQuery("skills", queryVec, 3)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Key)
	}
	return out
}
