package agent

import (
	"context"
	"encoding/json"
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
		Description: "Store a file by path and content.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
	},
	{
		Name:        "file_fetch",
		DisplayName: "File Fetch",
		Description: "Fetch a stored file by path.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
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
		Description: "Full-text search over stored file content.",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}`),
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
	{
		Name:        "file_search_semantic",
		DisplayName: "Semantic Search",
		Description: "Embed a query and search stored files by semantic similarity.",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"top_k":{"type":"integer"}}}`),
	},
}

// RegisterMemoryTools registers KV, file, and (when embedder is non-nil)
// vector/semantic tools into d. protectedPrefixes lists KV key prefixes that
// memory_delete will refuse to touch.
func RegisterMemoryTools(d *Dispatcher, store *memory.Store, embedder embed.Embedder, protectedPrefixes []string) {
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

		d.handlers["file_search_semantic"] = func(_ context.Context, args json.RawMessage) (string, error) {
			var req struct {
				Query string `json:"query"`
				TopK  int    `json:"top_k"`
			}
			if err := json.Unmarshal(args, &req); err != nil {
				return "", fmt.Errorf("file_search_semantic: %w", err)
			}
			if req.TopK <= 0 {
				req.TopK = 5
			}
			vec, err := embedder.Embed(context.Background(), req.Query)
			if err != nil {
				return "", fmt.Errorf("embed: %w", err)
			}
			results, err := store.VectorQuery("files", vec, req.TopK)
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
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_store: %w", err)
		}
		if err := store.FileStore(req.Path, req.Content); err != nil {
			return "", err
		}
		return "ok", nil
	}

	d.handlers["file_fetch"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_fetch: %w", err)
		}
		content, found, err := store.FileFetch(req.Path)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("file not found: %s", req.Path)
		}
		return content, nil
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
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("file_search_text: %w", err)
		}
		return store.FileSearchTextJSON(req.Query, req.Limit)
	}
}
