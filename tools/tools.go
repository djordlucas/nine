//go:build tools

// Package tools pins the code-generation binaries this repository runs. The
// blank import is what keeps them in go.mod; nothing here is compiled into
// nine itself.
package tools

import (
	_ "github.com/daveshanley/vacuum"
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
