// Package spec embeds the project specification so `nine spec <topic>` can serve
// it from the binary, keeping the bundled specs in lockstep with the sources.
package spec

import "embed"

// FS holds every Markdown file under spec/ — the top-level documents and the
// per-component contracts/ — embedded at build time.
//
//go:embed *.md contracts/*.md
var FS embed.FS
