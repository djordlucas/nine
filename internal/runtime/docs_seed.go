package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"nine/internal/docindex"
	"nine/internal/embed"
	"nine/internal/memory"
)

// docsFingerprintKey records the corpus + embedder fingerprint of the current
// index. It lives under the protected "self/" prefix: Nine can read it when
// reasoning about its own state but cannot delete it out from under the seeder.
const docsFingerprintKey = "self/docs-index-fingerprint"

// SeedDocs indexes the embedded docs and spec into memory.DocsNamespace so doc_search
// can rank them. Like the skill seeders it runs on every boot with the binary
// as the source of truth, but it is fingerprinted rather than unconditional:
// embedding a few hundred sections is one cheap local pass under the default
// keyword embedder and a few hundred HTTP round trips under Ollama, and the
// corpus only changes when the binary does. When the fingerprint matches, boot
// costs one KV read.
//
// embedderID identifies the embedding provider/model. It is part of the
// fingerprint because vectors from one embedder are meaningless to another, so
// switching providers must force a rebuild even though the docs did not change.
// A nil embedder skips indexing entirely — there is nothing to rank against,
// and the builder omits doc_search from the advertised set to match.
func SeedDocs(store *memory.Store, embedder embed.Embedder, embedderID string) error {
	if embedder == nil {
		return nil
	}
	sections, err := docindex.Sections()
	if err != nil {
		return fmt.Errorf("split bundled docs: %w", err)
	}

	want := docsFingerprint(sections, embedderID)
	if got, found, err := store.Get(docsFingerprintKey); err == nil && found && got == want {
		slog.Info("docs index up to date", "sections", len(sections))
		return nil
	}

	// The namespace is derived state, so rebuild it wholesale rather than
	// reconciling: a section that was renamed, merged, or split leaves an
	// address behind that no longer resolves, and a stale hit is worse than a
	// missing one — it sends the agent to read a section that is not there.
	if _, err := store.VectorDeleteNamespace(memory.DocsNamespace); err != nil {
		return fmt.Errorf("clear docs index: %w", err)
	}

	var indexed, failed int
	for _, s := range sections {
		vec, err := embedder.Embed(context.Background(), s.EmbedText())
		if err != nil {
			failed++
			continue
		}
		if err := store.VectorStore(memory.DocsNamespace+":"+s.Addr, memory.DocsNamespace, s.Addr, vec); err != nil {
			return fmt.Errorf("index %s: %w", s.Addr, err)
		}
		indexed++
	}

	// Only claim the fingerprint for a complete index. A partial pass (an
	// embedder that went away mid-run) must retry on the next boot rather than
	// leaving Nine with a half-searchable manual it believes is whole.
	if failed == 0 {
		if err := store.Set(docsFingerprintKey, want); err != nil {
			return fmt.Errorf("record docs fingerprint: %w", err)
		}
	}
	slog.Info("indexed bundled docs", "sections", indexed, "failed", failed)
	return nil
}

// docsFingerprint hashes every section address and body together with the
// embedder identity, so any edit, addition, removal, or provider switch changes
// it. Sections arrive in a deterministic order (bundles, then topics sorted by
// name, then document order), so the hash is stable across boots.
func docsFingerprint(sections []docindex.Section, embedderID string) string {
	h := sha256.New()
	fmt.Fprintf(h, "embedder:%s\n", embedderID)
	for _, s := range sections {
		fmt.Fprintf(h, "%s\n%s\n", s.Addr, s.Body)
	}
	return hex.EncodeToString(h.Sum(nil))
}
