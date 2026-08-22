package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"time"

	"nine/internal/agent"
	"nine/internal/memory"
)

// SpillRetention is how long a spilled tool output is kept before the sweep
// removes it. Spills are session debris — useful while the session that
// produced them is running and for a while after, for `nine trace` to resolve a
// journal reference, but not forever.
const SpillRetention = 7 * 24 * time.Hour

// spillSweepInterval is how often the daemon runs the retention sweep.
const spillSweepInterval = time.Hour

// registerLargeOutput wires the dispatcher's two large-output paths to the
// memory store (adr/tool-output-spill.md):
//
//   - the spill sink, which writes an over-cap tool result to the file store
//     under spill/<agentID>/ and hands the model back a path;
//   - the ref resolver, which reads a stored path back into the arguments of a
//     tool that declared an x-nine-ref parameter.
//
// Together they let a large payload move from one tool to another entirely
// inside the daemon: the model routes it by handle and never spends context on
// the bytes.
func registerLargeOutput(d *agent.Dispatcher, store *memory.Store, agentID string) {
	if store == nil {
		return
	}

	d.SetSpill(func(_ context.Context, toolName, output string) (string, error) {
		p := spillPath(agentID, toolName)
		if err := store.FileStore(p, output); err != nil {
			return "", fmt.Errorf("storing spilled output: %w", err)
		}
		return p, nil
	})

	d.SetRefResolver(func(_ context.Context, p string) (string, error) {
		content, found, err := store.FileFetch(p)
		if err != nil {
			return "", err
		}
		if !found {
			// Same wording the file tools use for an unreadable path, so a model
			// learns one lesson about the two namespaces rather than two.
			return "", agent.MissingStorePathError(store, p)
		}
		return content, nil
	})
}

// spillPath builds the file-store path for one spilled result:
// spill/<agentID>/<tool>-<random>.txt. The random suffix (rather than a
// counter) keeps paths unique across the parallel tool calls in a single turn
// without the sink needing shared state, and keeps them short enough that a
// model can copy one back accurately.
func spillPath(agentID, toolName string) string {
	var b [4]byte
	// A read failure only costs uniqueness, not correctness — the store would
	// overwrite an identical path — so a zero suffix is an acceptable fallback.
	_, _ = rand.Read(b[:])
	return path.Join(
		strings.TrimSuffix(agent.SpillPathPrefix, "/"),
		sanitizePathSegment(agentID),
		fmt.Sprintf("%s-%s.txt", sanitizePathSegment(toolName), hex.EncodeToString(b[:])),
	)
}

// sanitizePathSegment keeps a path segment to characters that read cleanly in a
// tool result and cannot introduce a directory boundary.
func sanitizePathSegment(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// sweepSpills deletes spilled outputs older than SpillRetention. Errors are
// logged, not returned: retention is housekeeping and must never take the
// daemon down.
func sweepSpills(store *memory.Store) {
	n, err := store.FileDeleteOlderThan(agent.SpillPathPrefix, SpillRetention)
	if err != nil {
		slog.Warn("spill sweep failed", "err", err)
		return
	}
	if n > 0 {
		slog.Info("swept expired tool-output spills", "deleted", n)
	}
}

// sweepToolState reclaims expired durable-state rows (spec/contracts/toolvm.md
// R-TVM.18).
//
// It rides on the spill sweeper's tick rather than getting a goroutine of its
// own: both are periodic housekeeping over the same store on the same cadence,
// and a second ticker would buy nothing.
//
// Expiry is applied on read as well, so a `ttl` is honoured between sweeps and
// this is purely about reclaiming the space. Doing it the other way round —
// reclaiming on read — would let a tool keep a value alive forever by never
// looking at it, which is the opposite of what a ttl is for.
func sweepToolState(store *memory.Store) {
	n, err := store.ToolStateExpire()
	if err != nil {
		slog.Warn("tool state sweep failed", "err", err)
		return
	}
	if n > 0 {
		slog.Info("swept expired tool state", "deleted", n)
	}
}

// RunSpillSweeper sweeps expired tool-output spills and expired durable tool
// state, once at startup and then on every tick until ctx is cancelled.
// Production-only housekeeping: the eval harness builds its store per case and
// throws it away, so it never starts one.
func RunSpillSweeper(ctx context.Context, store *memory.Store) {
	if store == nil {
		return
	}
	sweep := func() {
		sweepSpills(store)
		sweepToolState(store)
	}
	sweep()
	t := time.NewTicker(spillSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
