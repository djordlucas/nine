package toolvm

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
)

// The shipped tier: first-party tools compiled into the `nine` binary.
//
// This is the third source tier, after developer (a file and manifest on disk,
// R-TVM.10) and generated (a row in `tools`, R-TVM.14). It exists so that the
// capabilities Nine ships with go through the same sandbox as everything else.
//
// The tools here were built-in *plugins*: subprocesses running with the daemon's
// full uid authority. A plugin that reads a clock had, in principle, the reach to
// read the operator's home directory — not because anyone wanted that, but
// because a subprocess inherits it. Moving them under the capability model
// replaces ambient authority with a declared, operator-visible grant, which is
// what the model exists for (F7 in adr/architecture-review.md).
//
// A shipped tool is granted what it declares. That is the one way this tier
// differs from the other two, and it is not a weakening: a developer tool is
// granted by an operator who did not write it, and a generated tool is capped by
// a ceiling because Nine wrote it. A shipped tool is first-party code the
// operator already ran as a plugin with strictly more authority, so the grant is
// a **reduction** made explicit rather than a new trust. It is still visible in
// `nine tools`, and still refused if it declares something the host cannot
// confer.

//go:embed shipped/*.js
var shippedFS embed.FS

// shippedTool is one first-party tool: its manifest, inline, beside the source
// file it names.
type shippedTool struct {
	Name        string
	DisplayName string
	Description string
	Schema      string // JSON Schema for the arguments
	File        string // path within shippedFS
	// Declaration is what this tool needs. An empty Declaration means no
	// filesystem pre-open and no host functions beyond the ABI — the tool can
	// compute and nothing else.
	Declaration Declaration
	// AllowHosts is the net.http host allowlist, when the tool declares net.
	// First-party tools name their own, because they are the only ones that know:
	// web_search talks to three search endpoints, while the fetching tools exist
	// to retrieve whatever URL a model chose and therefore need "*".
	AllowHosts []string
	// Methods is the permitted HTTP method allowlist. Empty means GET only.
	Methods []string
}

// shippedTools is the catalog. Adding one is this entry plus its .js file.
var shippedTools = []shippedTool{
	{
		Name:        "time",
		DisplayName: "Get Time",
		Description: "Return the current date and time in UTC (ISO-8601 and a readable form).",
		Schema:      `{"type":"object","properties":{}}`,
		File:        "shipped/time.js",
		// Reading a clock needs nothing. This is the whole point of the
		// migration: as a plugin it had the daemon's authority, and as a tool it
		// has none.
		Declaration: Declaration{},
	},
	{
		Name:        "read_file",
		DisplayName: "Read File",
		Description: "Read a file from the WORKSPACE FILESYSTEM. Paths resolve under /work; a relative path is taken as relative to it. Read part of a large file with lines (\"120-180\") or with offset and limit, rather than pulling the whole thing into context. Pass line_numbers when you intend to edit, so you can name the exact text for edit_file. A windowed read also returns a version token for write_file's if_unchanged. This is not the memory file store — use file_fetch for a path you stored with file_store.",
		Schema: `{"type":"object","required":["path"],"properties":{
			"path":{"type":"string","description":"Path under the workspace, e.g. notes.txt or /work/notes.txt"},
			"lines":{"type":"string","description":"1-based inclusive line range, e.g. \"120-180\" or \"120\". Preferred over offset/limit for text."},
			"offset":{"type":"integer","description":"Byte offset to start at (default 0)."},
			"limit":{"type":"integer","description":"Maximum bytes to return; omit or 0 for the rest of the file."},
			"line_numbers":{"type":"boolean","description":"Prefix each line with its number."}
		}}`,
		File:        "shipped/read_file.js",
		Declaration: Declaration{FS: []string{"read"}},
	},
	{
		Name:        "write_file",
		DisplayName: "Write File",
		// Names file_store explicitly. Both tools answer to "store a file at this
		// path", and the memory file store already disambiguates itself against
		// read_file — the pointer needs to go both ways, or a prompt phrased as
		// "store two files" lands here and the searchable copy is never made.
		Description: "Write content to a file on the WORKSPACE FILESYSTEM, creating parent directories as needed. Paths resolve under /work. Replaces the file whole: to change part of an existing file use edit_file, and to add to the end pass mode=append. This is not the memory file store: a file written here is not full-text searchable and file_search_text will not find it — use file_store for anything you intend to look up later.",
		Schema: `{"type":"object","required":["path","content"],"properties":{
			"path":{"type":"string"},
			"content":{"type":"string"},
			"mode":{"type":"string","enum":["replace","append"],"description":"replace (default) rewrites the file; append adds to the end without reading it."},
			"if_unchanged":{"type":"string","description":"A version token from read_file. The write fails if the file changed since then."},
			"preview":{"type":"boolean","description":"Return the diff this write would make and write nothing."}
		}}`,
		File:        "shipped/write_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "edit_file",
		DisplayName: "Edit File",
		Description: "Replace exact text in a WORKSPACE FILESYSTEM file, leaving the rest byte-for-byte unchanged. Use this rather than write_file whenever the file already exists: it never loads the file into your context, so it works on a file far larger than your context window. old_text must match the file exactly, including indentation and line breaks — read the region with read_file(lines) first. The call fails, changing nothing, when the number of matches is not what you said to expect.",
		Schema: `{"type":"object","required":["path","old_text","new_text"],"properties":{
			"path":{"type":"string"},
			"old_text":{"type":"string","description":"Exact text to replace. Include enough surrounding lines to make it unique."},
			"new_text":{"type":"string","description":"Replacement text. Empty deletes the matched text."},
			"expect":{"description":"How many occurrences to replace: a positive integer (default 1), or \"all\"."},
			"preview":{"type":"boolean","description":"Return the diff this edit would make and write nothing."}
		}}`,
		File:        "shipped/edit_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "move_file",
		DisplayName: "Move File",
		Description: "Move or rename a file inside the WORKSPACE FILESYSTEM. The file is relinked, not copied and not read, so this costs nothing for a large file and never spends context on its contents. Refuses an existing destination unless overwrite is true.",
		Schema: `{"type":"object","required":["from","to"],"properties":{
			"from":{"type":"string"},
			"to":{"type":"string"},
			"overwrite":{"type":"boolean","description":"Replace the destination if it exists (default false)."}
		}}`,
		File:        "shipped/move_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "copy_file",
		DisplayName: "Copy File",
		Description: "Copy a file inside the WORKSPACE FILESYSTEM. The contents stream from one path to the other without passing through your context, so a large file costs nothing to duplicate. Refuses an existing destination unless overwrite is true.",
		Schema: `{"type":"object","required":["from","to"],"properties":{
			"from":{"type":"string"},
			"to":{"type":"string"},
			"overwrite":{"type":"boolean","description":"Replace the destination if it exists (default false)."}
		}}`,
		File:        "shipped/copy_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "delete_file",
		DisplayName: "Delete File",
		Description: "Delete one file, or one empty directory, from the WORKSPACE FILESYSTEM. The file is moved to Nine's trash rather than destroyed, so a mistake can be undone with trash_list and restore_file until the trash is swept. There is no recursive delete: delete a directory's contents first. Prefer this over `rm` in the shell, which destroys a file outright.",
		Schema:      `{"type":"object","required":["path"],"properties":{"path":{"type":"string"}}}`,
		File:        "shipped/delete_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "trash_list",
		DisplayName: "List Trash",
		Description: "List files recoverable from the trash, newest first: everything delete_file removed and every version write_file or edit_file replaced. Pass path to narrow to a path fragment. Each result names a trash entry to hand to restore_file.",
		Schema: `{"type":"object","properties":{
			"path":{"type":"string","description":"Only entries whose original path contains this text."},
			"limit":{"type":"integer","description":"Maximum entries to return (default 50)."}
		}}`,
		File:        "shipped/trash_list.js",
		Declaration: Declaration{FS: []string{"read"}},
	},
	{
		Name:        "restore_file",
		DisplayName: "Restore File",
		Description: "Restore a file from the trash to the workspace, by the entry name trash_list reports. Restores to its original path unless to names another. Never overwrites: if something already occupies the destination the call fails, since recovering one file by destroying another is not a recovery.",
		Schema: `{"type":"object","required":["entry"],"properties":{
			"entry":{"type":"string","description":"Trash entry name from trash_list, e.g. 20260920T143015Z-1f2e3d4c."},
			"path":{"type":"string","description":"Which file to restore, when the entry holds more than one."},
			"to":{"type":"string","description":"Destination path; defaults to where the file came from."}
		}}`,
		File:        "shipped/restore_file.js",
		Declaration: Declaration{FS: []string{"write"}},
	},
	{
		Name:        "diff_file",
		DisplayName: "Diff File",
		Description: "Show what changed in a workspace file, as a unified diff against the version before the most recent change. Use it to report an edit back to the person you are working for, rather than describing the change in prose. Only a file that write_file, edit_file or delete_file has changed has a previous version; pass against with a trash entry name from trash_list to compare with an older one.",
		Schema: `{"type":"object","required":["path"],"properties":{
			"path":{"type":"string"},
			"against":{"type":"string","description":"Trash entry name to compare against; defaults to the most recent version of this file."}
		}}`,
		File:        "shipped/diff_file.js",
		Declaration: Declaration{FS: []string{"read"}},
	},
	{
		Name:        "http_get",
		DisplayName: "HTTP GET",
		Description: "Fetch a URL and return its status and body.",
		Schema:      `{"type":"object","required":["url"],"properties":{"url":{"type":"string"},"headers":{"type":"object"}}}`,
		File:        "shipped/http_get.js",
		Declaration: Declaration{Net: []string{"http"}},
		// The tool exists to fetch whatever URL the model chose, which no host
		// list expresses. "*" grants any host; the dial-time address checks still
		// refuse loopback, link-local, private ranges and multicast (R-TVM.12).
		AllowHosts: []string{"*"},
		Methods:    []string{"GET"},
	},
	{
		Name:        "http_post",
		DisplayName: "HTTP POST",
		Description: "POST a body to a URL and return the status and response.",
		Schema:      `{"type":"object","required":["url"],"properties":{"url":{"type":"string"},"body":{},"headers":{"type":"object"}}}`,
		File:        "shipped/http_post.js",
		Declaration: Declaration{Net: []string{"http"}},
		AllowHosts:  []string{"*"},
		Methods:     []string{"POST"},
	},
	{
		Name:        "web_page_read",
		DisplayName: "Read Web Page",
		Description: "Fetch a web page and return its readable text, with markup and scripts stripped.",
		Schema:      `{"type":"object","required":["url"],"properties":{"url":{"type":"string"}}}`,
		File:        "shipped/web_page_read.js",
		Declaration: Declaration{Net: []string{"http"}},
		AllowHosts:  []string{"*"},
		Methods:     []string{"GET"},
	},
	{
		Name:        "web_search",
		DisplayName: "Web Search",
		Description: "Search the web and return result titles, URLs, and snippets. Uses DuckDuckGo by default; set SEARCH_PROVIDER=brave|serpapi and SEARCH_API_KEY for another backend.",
		Schema:      `{"type":"object","required":["query"],"properties":{"query":{"type":"string"},"limit":{"type":"integer"}}}`,
		File:        "shipped/web_search.js",
		// Unlike the fetching tools this one has a real allowlist: three known
		// search endpoints. The tool that can be constrained is constrained.
		Declaration: Declaration{Net: []string{"http"}, Env: []string{"SEARCH_PROVIDER", "SEARCH_API_KEY"}},
		AllowHosts:  []string{"html.duckduckgo.com", "api.search.brave.com", "serpapi.com"},
		Methods:     []string{"GET"},
	},
}

// ShippedWorkspace is the host directory a shipped tool's fs grant points at.
//
// The guest path is fixed at /work because that is the alias the `files` plugin
// already accepted, so a model that learned "/work/notes.txt" keeps working. The
// host side comes from the daemon (NINE_WORKSPACE, or the eval harness's
// per-case dir). Empty means no workspace, and a tool declaring fs then fails to
// load rather than registering with a capability that silently does nothing.
type ShippedWorkspace struct{ Host string }

// shippedWorkspaceGuest is where the workspace appears inside the sandbox.
const shippedWorkspaceGuest = "/work"

// SetShippedWorkspace installs the mount used by shipped tools that declare fs.
// Must be called before LoadShipped.
func (h *Host) SetShippedWorkspace(w ShippedWorkspace) {
	h.mu.Lock()
	h.shippedWorkspace = w
	h.mu.Unlock()
}

// LoadShipped registers the first-party tools compiled into the binary.
//
// It runs before Load and LoadGenerated so that a developer or generated tool
// cannot take a shipped tool's name — the namespace rule is first-registered
// wins, and a shipped tool losing its name to a later one would silently replace
// first-party behavior.
func (h *Host) LoadShipped(ctx context.Context, collides Collides) {
	var status []Status
	for _, s := range shippedTools {
		st := Status{Name: s.Name, Kind: string(KindJS), Shipped: true}

		src, err := shippedFS.ReadFile(s.File)
		if err != nil {
			// Unreachable in a correct build — go:embed would have failed — so
			// this is a guard against a catalog entry naming a file nobody added.
			status = append(status, skip(st, fmt.Errorf("shipped source %s: %w", s.File, err), "read"))
			continue
		}

		h.mu.RLock()
		ws := h.shippedWorkspace
		h.mu.RUnlock()

		grant, err := resolveShipped(s.Declaration, ws, s.AllowHosts, s.Methods)
		if err != nil {
			status = append(status, skip(st, err, "grant"))
			continue
		}
		st.Capabilities = grant.Summary()

		if collides != nil {
			if owner, taken := collides(s.Name); taken {
				status = append(status, skip(st,
					fmt.Errorf("tool %q already provided by %q", s.Name, owner), "collision"))
				continue
			}
		}

		t := &Tool{
			Name:        s.Name,
			DisplayName: s.DisplayName,
			Description: s.Description,
			InputSchema: json.RawMessage(s.Schema),
			Kind:        KindJS,
			Grant:       grant,
			Shipped:     true,
			module:      h.qjs,
			source:      string(src),
			imports:     stdlibModules(),
		}

		h.mu.Lock()
		h.tools[s.Name] = t
		h.mu.Unlock()

		st.Loaded = true
		status = append(status, st)
	}

	h.mu.Lock()
	h.shippedStatus = status
	h.mu.Unlock()

	loaded := 0
	for _, s := range status {
		if s.Loaded {
			loaded++
		}
	}
	slog.Info("loaded shipped tools", "loaded", loaded, "skipped", len(status)-loaded)
	_ = ctx
}

// resolveShipped turns a shipped tool's declaration into its grant.
//
// Unlike resolveGrant it has no operator table to consult — the declaration is
// the grant — but it keeps the property that makes the capability model work:
// the host confers only what it can actually enforce, so a declaration naming
// something unsupported is refused rather than silently dropped. A tool that
// declares nothing gets nothing, which is the common case and the reason `time`
// was the right one to migrate first.
func resolveShipped(d Declaration, ws ShippedWorkspace, allowHosts, allowMethods []string) (Grant, error) {
	var g Grant
	if len(d.FS) > 0 {
		if ws.Host == "" {
			return Grant{}, fmt.Errorf("shipped tool declares fs %v but no workspace is configured", d.FS)
		}
		m := Mount{Host: ws.Host, Guest: shippedWorkspaceGuest}
		for _, verb := range d.FS {
			switch verb {
			case "read":
				g.FSRead = append(g.FSRead, m)
			case "write":
				// A write mount is readable too (nine:fs treats either grant as
				// admitting a read), so a tool that writes can read back what it
				// wrote — which the plugin could, and which write_file needs.
				g.FSWrite = append(g.FSWrite, m)
			default:
				return Grant{}, fmt.Errorf("unknown fs capability %q", verb)
			}
		}
	}
	for _, verb := range d.Net {
		if verb != "http" {
			return Grant{}, fmt.Errorf("unknown net capability %q", verb)
		}
		if len(allowHosts) == 0 {
			return Grant{}, errors.New("shipped tool declares net.http with no allow_hosts")
		}
		methods := allowMethods
		if len(methods) == 0 {
			methods = []string{"GET"}
		}
		g.HTTP = &HTTPGrant{AllowHosts: allowHosts, Methods: methods}
	}
	g.Env = append(g.Env, d.Env...)
	return g, nil
}
