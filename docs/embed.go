// Package docs embeds user-facing documentation so commands can render it at
// runtime. Embedding keeps the built-in help in lockstep with the Markdown
// sources — there is no second copy to keep in sync.
package docs

import "embed"

// FS holds every Markdown document under docs/, embedded at build time so
// `nine docs <topic>` can serve them without the source tree present.
//
//go:embed *.md
var FS embed.FS

// Usage is the full contents of usage.md, the CLI usage reference. It is the
// single source of truth rendered by `nine help`.
//
//go:embed usage.md
var Usage string
