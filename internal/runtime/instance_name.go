package runtime

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"strings"

	"nine/internal/llm"
)

// DefaultInstanceName is the placeholder shown in the TUI top bar before a real
// instance name has been resolved or generated.
const DefaultInstanceName = "nine"

// instanceNameKV is the store key under which a generated instance name is
// persisted so it stays stable across daemon restarts.
const instanceNameKV = "instance.name"

// maxInstanceNameLen bounds a generated or configured instance name so a
// pathological model reply cannot overflow the header.
const maxInstanceNameLen = 32

// InstanceNameStore is the slice of the memory store the daemon uses to persist
// and reuse a generated instance name.
type InstanceNameStore interface {
	Get(key string) (string, bool, error)
	Set(key, value string) error
}

// ResolveInstanceName determines this daemon's display name and applies it via
// SetInstanceName. Precedence:
//
//  1. A non-empty configName wins and is authoritative (nothing is persisted).
//  2. Otherwise a name previously generated and persisted in store is reused.
//  3. Otherwise the daemon takes DefaultInstanceName immediately and, in the
//     background, asks the LLM to coin a random name, persists it, and pushes
//     it live to connected clients.
//
// It never blocks: the LLM call runs in its own goroutine. A nil provider or
// store degrades gracefully (the placeholder simply stays).
func (d *Daemon) ResolveInstanceName(ctx context.Context, configName string, store InstanceNameStore, provider llm.Provider) {
	if name := sanitizeInstanceName(configName); name != "" {
		d.SetInstanceName(name)
		return
	}
	if store != nil {
		if v, ok, err := store.Get(instanceNameKV); err != nil {
			slog.Warn("read persisted instance name", "err", err)
		} else if name := sanitizeInstanceName(v); ok && name != "" {
			d.SetInstanceName(name)
			return
		}
	}

	// First boot with no configured or persisted name: show a placeholder now
	// and name the instance asynchronously.
	d.SetInstanceName(DefaultInstanceName)
	if provider == nil {
		return
	}
	go func() {
		name := generateInstanceName(ctx, provider)
		if name == "" {
			name = randomInstanceName()
		}
		if store != nil {
			if err := store.Set(instanceNameKV, name); err != nil {
				slog.Warn("persist generated instance name", "name", name, "err", err)
			}
		}
		d.SetInstanceName(name)
		slog.Info("named this nine instance", "name", name)
	}()
}

// instanceNamePrompt asks the model for a single short, memorable name.
const instanceNamePrompt = `You are naming a personal AI agent instance. ` +
	`Invent one short, memorable, friendly name — a single word or two, like a code name or a pet name. ` +
	`Reply with ONLY the name: no quotes, no punctuation, no explanation.`

// generateInstanceName asks the LLM for a random instance name. Returns "" on
// any error or an unusable reply so the caller can fall back.
func generateInstanceName(ctx context.Context, provider llm.Provider) string {
	resp, err := provider.Complete(ctx, llm.Request{
		System:    instanceNamePrompt,
		Messages:  []llm.Message{{Role: "user", Text: "Name this instance."}},
		MaxTokens: 32,
	})
	if err != nil {
		slog.Warn("generate instance name", "err", err)
		return ""
	}
	return sanitizeInstanceName(resp.Text)
}

// sanitizeInstanceName trims a raw name to a single clean line: it takes the
// first non-empty line, strips surrounding quotes, collapses to a bounded
// length, and returns "" when nothing usable remains.
func sanitizeInstanceName(raw string) string {
	line := raw
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	line = strings.Trim(line, `"'`+"`.,")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	if len(line) > maxInstanceNameLen {
		line = strings.TrimSpace(line[:maxInstanceNameLen])
	}
	return line
}

// instanceNameAdjectives and instanceNameNouns seed the offline fallback name.
var (
	instanceNameAdjectives = []string{
		"quiet", "amber", "clever", "brisk", "lucid", "steady", "nimble",
		"cobalt", "silent", "bright", "willow", "north", "ember", "swift",
	}
	instanceNameNouns = []string{
		"fox", "harbor", "cedar", "atlas", "river", "sparrow", "orbit",
		"lantern", "delta", "meadow", "cipher", "beacon", "vale", "comet",
	}
)

// randomInstanceName produces a deterministic-format "adjective-noun" fallback
// used when the LLM is unavailable, so an instance is still named on first boot.
func randomInstanceName() string {
	return pick(instanceNameAdjectives) + "-" + pick(instanceNameNouns)
}

// pick returns a cryptographically-random element of words (words must be
// non-empty), falling back to the first element if the RNG fails.
func pick(words []string) string {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(words))))
	if err != nil {
		return words[0]
	}
	return words[n.Int64()]
}
