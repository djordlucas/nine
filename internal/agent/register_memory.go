package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
)

var memoryToolDefs = []llm.ToolDef{
	{
		Name:        "memory_get",
		DisplayName: "Memory Get",
		Description: "Retrieve a value from the key-value store by key.",
		InputSchema: json.RawMessage(`{"type":"object","required":["key"],"properties":{"key":{"type":"string"}}}`),
	},
	{
		Name:        "memory_set",
		DisplayName: "Memory Set",
		Description: "Store a value in the key-value store.",
		InputSchema: json.RawMessage(`{"type":"object","required":["key","value"],"properties":{"key":{"type":"string"},"value":{"type":"string"}}}`),
	},
	{
		Name:        "memory_delete",
		DisplayName: "Memory Delete",
		Description: "Delete a key from the key-value store.",
		InputSchema: json.RawMessage(`{"type":"object","required":["key"],"properties":{"key":{"type":"string"}}}`),
	},
	{
		Name:        "memory_list",
		DisplayName: "Memory List",
		Description: "List keys in the key-value store, optionally filtered by prefix.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prefix":{"type":"string"}}}`),
	},
	{
		Name:        "file_store",
		DisplayName: "File Store",
		Description: "Store a file by path and content so you can find it again later — full-text searchable with file_search_text, and retrievable by path with file_fetch. This is your own memory file store, separate from the workspace filesystem that shell, read_file and write_file use; prefer it whenever you are keeping something to recall in a later turn. To copy an already-stored file — a spilled tool output, typically — to a new path without reading it, pass content_ref instead of content.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"content":{"type":"string"},"content_ref":{"type":"string","x-nine-ref":true,"description":"A MEMORY FILE-STORE path whose content to store at path, e.g. a spill/... path from a truncated tool result. NOT a filesystem path: a file created by shell or write_file is not in the store. Used instead of content; the data never passes through your context."}}}`),
	},
	{
		Name:        "file_fetch",
		DisplayName: "File Fetch",
		Description: "Fetch a stored file by path. For a large file, read it in windows with offset and limit rather than all at once — an over-large result is capped and spilled again.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer","description":"Character offset to start reading at (default 0)."},"limit":{"type":"integer","description":"Maximum characters to return; omit or 0 for the whole file."}}}`),
	},
	{
		Name:        "file_list",
		DisplayName: "File List",
		Description: "List stored files, optionally filtered by path prefix.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"prefix":{"type":"string"}}}`),
	},
	{
		Name:        "file_search_text",
		DisplayName: "File Search",
		Description: "Full-text search over stored file content. Pass path to search inside one file (or one directory prefix) — the way to find the relevant region of a large spilled tool output.",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"path":{"type":"string","description":"Restrict the search to paths starting with this prefix; omit to search all files."},"limit":{"type":"integer"}}}`),
	},
	{
		Name:        "memory_embed",
		DisplayName: "Embed Text",
		Description: "Embed text and store the resulting vector in the memory store under a namespace and key.",
		InputSchema: json.RawMessage(`{"type":"object","required":["id","namespace","key","text"],"properties":{"id":{"type":"string"},"namespace":{"type":"string"},"key":{"type":"string"},"text":{"type":"string"}}}`),
	},
	{
		Name:        "memory_query",
		DisplayName: "Memory Query",
		Description: "Embed a query and return the most semantically similar stored items from a namespace.",
		InputSchema: json.RawMessage(`{"type":"object","required":["namespace","query"],"properties":{"namespace":{"type":"string"},"query":{"type":"string"},"top_k":{"type":"integer","description":"Number of results (default 5)"}}}`),
	},
}

// RegisterMemoryTools registers KV, file, and (when embedder is non-nil)
// vector/semantic tools into d. protectedPrefixes lists KV key prefixes that
// memory_delete will refuse to touch.
//
// When indexMemories is true and an embedder is present, every memory_set is
// mirrored into the shared memory.MemoriesNamespace vector pool (embedding of
// the value, keyed by the KV key) and memory_delete removes the mirror, so the
// context builder can pull-surface memories relevant to a later turn. Indexing
// is best-effort: an embed/store failure never fails the underlying KV write
// (graceful degradation, embedder contract R-EMB.5).
func RegisterMemoryTools(d *Dispatcher, store *memory.Store, embedder embed.Embedder, protectedPrefixes []string, indexMemories bool) {
	if embedder != nil {
		d.handlers["memory_embed"] = func(_ context.Context, args json.RawMessage) (string, error) {
			var req struct {
				ID        string `json:"id"`
				Namespace string `json:"namespace"`
				Key       string `json:"key"`
				Text      string `json:"text"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("memory_embed: %w", err)
			}
			vec, err := embedder.Embed(context.Background(), req.Text)
			if err != nil {
				return "", fmt.Errorf("embed: %w", err)
			}
			if err := store.VectorStore(req.ID, req.Namespace, req.Key, vec); err != nil {
				return "", fmt.Errorf("vector store: %w", err)
			}
			return fmt.Sprintf("stored %d-dim vector for %s/%s", len(vec), req.Namespace, req.Key), nil
		}

		d.handlers["memory_query"] = func(_ context.Context, args json.RawMessage) (string, error) {
			var req struct {
				Namespace string `json:"namespace"`
				Query     string `json:"query"`
				TopK      int    `json:"top_k"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("memory_query: %w", err)
			}
			if req.TopK <= 0 {
				req.TopK = 5
			}
			vec, err := embedder.Embed(context.Background(), req.Query)
			if err != nil {
				return "", fmt.Errorf("embed: %w", err)
			}
			results, err := store.VectorQuery(req.Namespace, vec, req.TopK)
			if err != nil {
				return "", fmt.Errorf("vector query: %w", err)
			}
			data, err := json.Marshal(map[string]any{"results": results})
			if err != nil {
				return "", err
			}
			return string(data), nil
		}

	}

	d.handlers["memory_get"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("memory_get: %w", err)
		}
		val, found, err := store.Get(req.Key)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("key not found: %s", req.Key)
		}
		return val, nil
	}

	d.handlers["memory_set"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("memory_set: %w", err)
		}
		if err := store.Set(req.Key, req.Value); err != nil {
			return "", err
		}
		// Mirror the memory into the shared vector pool so later turns can
		// pull-surface it by relevance. Best-effort: a failure here must not
		// fail the KV write the agent asked for.
		if indexMemories && embedder != nil {
			if vec, err := embedder.Embed(context.Background(), req.Value); err == nil && len(vec) > 0 {
				_ = store.VectorStore("memories:"+req.Key, memory.MemoriesNamespace, req.Key, vec)
			}
		}
		return "ok", nil
	}

	d.handlers["memory_delete"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("memory_delete: %w", err)
		}
		for _, pfx := range protectedPrefixes {
			if strings.HasPrefix(req.Key, pfx) {
				return "", fmt.Errorf("key %q is protected and cannot be deleted; use memory_set to update it", req.Key)
			}
		}
		if err := store.Delete(req.Key); err != nil {
			return "", err
		}
		// Keep the vector pool in sync with the KV store. Best-effort.
		if indexMemories && embedder != nil {
			_ = store.VectorDelete("memories:" + req.Key)
		}
		return "ok", nil
	}

	d.handlers["memory_list"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Prefix string `json:"prefix"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("memory_list: %w", err)
		}
		return store.KVListString(req.Prefix)
	}

	d.handlers["file_store"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			// ContentRef arrives already expanded: the dispatcher replaced the
			// path the model supplied with the content stored there (refs.go).
			ContentRef string `json:"content_ref"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_store: %w", err)
		}
		if strings.HasPrefix(req.Path, SpillPathPrefix) {
			return "", fmt.Errorf("file_store: %q is reserved for spilled tool output and is not writable; choose another path", req.Path)
		}
		content := req.Content
		if content == "" && req.ContentRef != "" {
			content = req.ContentRef
		}
		if err := store.FileStore(req.Path, content); err != nil {
			return "", err
		}
		return "ok", nil
	}

	d.handlers["file_fetch"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_fetch: %w", err)
		}
		// A plain whole-file fetch stays a plain string result; only a windowed
		// read reports its position, which the model needs to page onwards.
		if req.Offset <= 0 && req.Limit <= 0 {
			content, found, err := store.FileFetch(req.Path)
			if err != nil {
				return "", err
			}
			if !found {
				return "", fmt.Errorf("file_fetch: %s", missingStorePath(store, req.Path))
			}
			return content, nil
		}
		slice, found, err := store.FileFetchRange(req.Path, req.Offset, req.Limit)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("file_fetch: %s", missingStorePath(store, req.Path))
		}
		data, err := json.Marshal(slice)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["file_list"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Prefix string `json:"prefix"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_list: %w", err)
		}
		return store.FileListString(req.Prefix)
	}

	d.handlers["file_search_text"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Query string `json:"query"`
			Path  string `json:"path"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_search_text: %w", err)
		}
		results, err := store.FileSearchTextScoped(req.Query, req.Path, req.Limit)
		if err != nil {
			return "", err
		}
		// A bare "null" for no hits teaches the model nothing, and a live model
		// answered it by inventing a value. Say why there were none — and if the
		// path filter matched nothing at all, say so explicitly, since the usual
		// cause is a workspace path used where a store path belongs.
		if len(results) == 0 {
			return noSearchHitsMessage(store, req.Query, req.Path), nil
		}
		data, err := json.Marshal(results)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// noSearchHitsMessage explains an empty file_search_text result. Nine has two
// file namespaces — the workspace filesystem (shell, read_file, write_file) and
// the memory file store (file_store, file_fetch, spilled output) — and a model
// that searches the wrong one gets zero hits for a reason it cannot see.
func noSearchHitsMessage(store *memory.Store, query, pathFilter string) string {
	if pathFilter == "" {
		return fmt.Sprintf("No stored file matched %q. The memory file store may be empty, "+
			"or the text may be in the workspace filesystem instead (try shell/read_file).", query)
	}
	paths, err := store.FileList(pathFilter)
	if err == nil && len(paths) == 0 {
		msg := fmt.Sprintf("No file is stored under %q, so nothing could be searched.", pathFilter)
		if strings.HasPrefix(pathFilter, "/") || strings.HasPrefix(pathFilter, "./") {
			msg += " That looks like a workspace filesystem path; this tool searches the" +
				" MEMORY FILE STORE, whose paths have no leading slash (for example a" +
				" spill/... path from a truncated tool result)."
		}
		if stored, lerr := store.FileList(""); lerr == nil && len(stored) > 0 {
			if len(stored) > searchHintLimit {
				stored = stored[:searchHintLimit]
			}
			msg += " Stored paths available now: " + strings.Join(stored, ", ")
		}
		return msg
	}
	return fmt.Sprintf("No text matching %q was found in the file(s) under %q.", query, pathFilter)
}

// searchHintLimit caps how many stored paths an empty-result message lists, so
// the hint cannot itself blow the output cap.
const searchHintLimit = 20

// MissingStorePathError reports a path absent from the memory file store, with
// the namespace explanation missingStorePath builds. Exported so the ref
// resolver (internal/runtime) reports an unresolvable x-nine-ref argument the
// same way the file tools report an unreadable path — one wording, one lesson.
func MissingStorePathError(store *memory.Store, path string) error {
	return errors.New(missingStorePath(store, path))
}

// missingStorePath explains a path that is not in the memory file store,
// naming the namespace and — when the path looks like a workspace filesystem
// path — saying so outright. A live model retrieved a spill by passing its
// original /work/... path here and, given a bare "not found", invented an
// answer instead of correcting itself.
func missingStorePath(store *memory.Store, path string) string {
	msg := fmt.Sprintf("no file stored at %q in the memory file store", path)
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "./") {
		msg += ". That looks like a workspace filesystem path; this tool reads the" +
			" MEMORY FILE STORE, whose paths have no leading slash (for example a" +
			" spill/... path named in a truncation notice). Use read_file for a" +
			" workspace file"
	}
	if stored, err := store.FileList(""); err == nil && len(stored) > 0 {
		if len(stored) > searchHintLimit {
			stored = stored[:searchHintLimit]
		}
		msg += ". Stored paths available now: " + strings.Join(stored, ", ")
	}
	return msg
}
