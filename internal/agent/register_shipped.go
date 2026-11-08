package agent

import (
	"context"
	"encoding/json"

	"nine/internal/llm"
)

// The shipped tier, translated to Go (adr/modular_nine.md §9.10): the tools Nine
// ships are plain in-process dispatcher handlers now — direct Go calls, no
// sandbox host — because the toolvm is being removed. Names, descriptions and
// schemas are carried over unchanged from the shipped catalog so the model sees
// exactly what it saw before. The two Live process programs (pursue, reflect)
// are NOT here; they stay on the toolvm host until its removal completes.

// shippedToolDefs are the advertised definitions of the Go shipped tools.
var shippedToolDefs = []llm.ToolDef{
	{
		Name:        "time",
		DisplayName: "Get Time",
		Description: "Return the current date and time in UTC (ISO-8601 and a readable form).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "read_file",
		DisplayName: "Read File",
		Description: "Read a file from the WORKSPACE FILESYSTEM. Paths resolve under /work; a relative path is taken as relative to it. Read part of a large file with lines (\"120-180\") or with offset and limit, rather than pulling the whole thing into context. Pass line_numbers when you intend to edit, so you can name the exact text for edit_file. A windowed read also returns a version token for write_file's if_unchanged. A spill/... path from a truncated tool result is read with this same tool.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{
			"path":{"type":"string","description":"Path under the workspace, e.g. notes.txt or /work/notes.txt"},
			"lines":{"type":"string","description":"1-based inclusive line range, e.g. \"120-180\" or \"120\". Preferred over offset/limit for text."},
			"offset":{"type":"integer","description":"Byte offset to start at (default 0)."},
			"limit":{"type":"integer","description":"Maximum bytes to return; omit or 0 for the rest of the file."},
			"line_numbers":{"type":"boolean","description":"Prefix each line with its number."}
		}}`),
	},
	{
		Name:        "write_file",
		DisplayName: "Write File",
		Description: "Write content to a file on the WORKSPACE FILESYSTEM, creating parent directories as needed. To copy a large payload — a spilled tool result, typically — pass content_ref with its path instead of content, and the bytes never enter your context. Paths resolve under /work. Replaces the file whole: to change part of an existing file use edit_file, and to add to the end pass mode=append. A file written here is full-text searchable with file_search_text once the workspace scan sees it.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{
			"path":{"type":"string"},
			"content":{"type":"string"},
			"mode":{"type":"string","enum":["replace","append"],"description":"replace (default) rewrites the file; append adds to the end without reading it."},
			"if_unchanged":{"type":"string","description":"A version token from read_file. The write fails if the file changed since then."},
			"preview":{"type":"boolean","description":"Return the diff this write would make and write nothing."},
			"content_ref":{"type":"string","x-nine-ref":true,"description":"A path whose content to write here — a spill/... path from a truncated tool result, or another workspace file. Used instead of content; the data never passes through your context."}
		}}`),
	},
	{
		Name:        "edit_file",
		DisplayName: "Edit File",
		Description: "Replace exact text in a WORKSPACE FILESYSTEM file, leaving the rest byte-for-byte unchanged. Use this rather than write_file whenever the file already exists: it never loads the file into your context, so it works on a file far larger than your context window. old_text must match the file exactly, including indentation and line breaks — read the region with read_file(lines) first. The call fails, changing nothing, when the number of matches is not what you said to expect.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","old_text","new_text"],"properties":{
			"path":{"type":"string"},
			"old_text":{"type":"string","description":"Exact text to replace. Include enough surrounding lines to make it unique."},
			"new_text":{"type":"string","description":"Replacement text. Empty deletes the matched text."},
			"expect":{"description":"How many occurrences to replace: a positive integer (default 1), or \"all\"."},
			"preview":{"type":"boolean","description":"Return the diff this edit would make and write nothing."}
		}}`),
	},
	{
		Name:        "move_file",
		DisplayName: "Move File",
		Description: "Move or rename a file inside the WORKSPACE FILESYSTEM. The file is relinked, not copied and not read, so this costs nothing for a large file and never spends context on its contents. Refuses an existing destination unless overwrite is true.",
		InputSchema: json.RawMessage(`{"type":"object","required":["from","to"],"properties":{
			"from":{"type":"string"},
			"to":{"type":"string"},
			"overwrite":{"type":"boolean","description":"Replace the destination if it exists (default false)."}
		}}`),
	},
	{
		Name:        "copy_file",
		DisplayName: "Copy File",
		Description: "Copy a file inside the WORKSPACE FILESYSTEM. The contents stream from one path to the other without passing through your context, so a large file costs nothing to duplicate. Refuses an existing destination unless overwrite is true.",
		InputSchema: json.RawMessage(`{"type":"object","required":["from","to"],"properties":{
			"from":{"type":"string"},
			"to":{"type":"string"},
			"overwrite":{"type":"boolean","description":"Replace the destination if it exists (default false)."}
		}}`),
	},
	{
		Name:        "delete_file",
		DisplayName: "Delete File",
		Description: "Delete one file, or one empty directory, from the WORKSPACE FILESYSTEM. The file is moved to Nine's trash rather than destroyed, so a mistake can be undone with trash_list and restore_file until the trash is swept. There is no recursive delete: delete a directory's contents first. Prefer this over `rm` in the shell, which destroys a file outright.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`),
	},
	{
		Name:        "trash_list",
		DisplayName: "List Trash",
		Description: "List files recoverable from the trash, newest first: everything delete_file removed and every version write_file or edit_file replaced. Pass path to narrow to a path fragment. Each result names a trash entry to hand to restore_file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"path":{"type":"string","description":"Only entries whose original path contains this text."},
			"limit":{"type":"integer","description":"Maximum entries to return (default 50)."}
		}}`),
	},
	{
		Name:        "restore_file",
		DisplayName: "Restore File",
		Description: "Restore a file from the trash to the workspace, by the entry name trash_list reports. Restores to its original path unless to names another. Never overwrites: if something already occupies the destination the call fails, since recovering one file by destroying another is not a recovery.",
		InputSchema: json.RawMessage(`{"type":"object","required":["entry"],"properties":{
			"entry":{"type":"string","description":"Trash entry name from trash_list, e.g. 20260920T143015Z-1f2e3d4c."},
			"path":{"type":"string","description":"Which file to restore, when the entry holds more than one."},
			"to":{"type":"string","description":"Destination path; defaults to where the file came from."}
		}}`),
	},
	{
		Name:        "diff_file",
		DisplayName: "Diff File",
		Description: "Show what changed in a workspace file, as a unified diff against the version before the most recent change. Use it to report an edit back to the person you are working for, rather than describing the change in prose. Only a file that write_file, edit_file or delete_file has changed has a previous version; pass against with a trash entry name from trash_list to compare with an older one.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{
			"path":{"type":"string"},
			"against":{"type":"string","description":"Trash entry name to compare against; defaults to the most recent version of this file."}
		}}`),
	},
	{
		Name:        "http_get",
		DisplayName: "HTTP GET",
		Description: "Fetch a URL and return its status and body.",
		InputSchema: json.RawMessage(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"},"headers":{"type":"object"}}}`),
	},
	{
		Name:        "http_post",
		DisplayName: "HTTP POST",
		Description: "POST a body to a URL and return the status and response.",
		InputSchema: json.RawMessage(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"},"body":{},"headers":{"type":"object"}}}`),
	},
	{
		Name:        "web_page_read",
		DisplayName: "Read Web Page",
		Description: "Fetch a web page and return its readable text, with markup and scripts stripped.",
		InputSchema: json.RawMessage(`{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}`),
	},
	{
		Name:        "web_search",
		DisplayName: "Web Search",
		Description: "Search the web and return result titles, URLs, and snippets. Uses DuckDuckGo by default; set SEARCH_PROVIDER=brave|serpapi and SEARCH_API_KEY for another backend.",
		InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}`),
	},
}

// ShippedDefs returns the shipped tools' definitions (adr/modular_nine.md §9.10).
func ShippedDefs() []llm.ToolDef {
	out := make([]llm.ToolDef, len(shippedToolDefs))
	copy(out, shippedToolDefs)
	return out
}

// ShippedToolNames returns the shipped tools' names, in catalog order.
func ShippedToolNames() []string {
	out := make([]string, 0, len(shippedToolDefs))
	for _, d := range shippedToolDefs {
		out = append(out, d.Name)
	}
	return out
}

// RegisterShippedTools registers the Go shipped tools on d as builtin handlers,
// confined to the workspace at root. Empty root registers handlers that explain
// the missing setting instead of failing as unknown tools.
func RegisterShippedTools(d *Dispatcher, root string) {
	fs := &shippedFS{root: root}
	handlers := map[string]func(context.Context, json.RawMessage) (string, error){
		"time":          shippedTime,
		"read_file":     fs.readFile,
		"write_file":    fs.writeFile,
		"edit_file":     fs.editFile,
		"move_file":     fs.moveFile,
		"copy_file":     fs.copyFile,
		"delete_file":   fs.deleteFile,
		"trash_list":    fs.trashList,
		"restore_file":  fs.restoreFile,
		"diff_file":     fs.diffFile,
		"http_get":      shippedHTTPGet,
		"http_post":     shippedHTTPPost,
		"web_page_read": shippedWebPageRead,
		"web_search":    shippedWebSearch,
	}
	for _, def := range shippedToolDefs {
		fn := handlers[def.Name]
		d.handlers[def.Name] = fn
		d.backends[def.Name] = "builtin"
		if len(def.InputSchema) > 0 {
			d.declareRefParams(def.Name, def.InputSchema)
		}
	}
}
