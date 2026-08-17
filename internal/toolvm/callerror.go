package toolvm

import (
	"fmt"
	"strings"
)

// CallError is a tool's own failure, with whatever structure it reported.
//
// The dispatcher hands the model an error's message and nothing else
// (internal/agent/dispatcher.go), so rendering is the only channel this
// structure has. That constrains the shape of Error() below: it must read as a
// sentence to a language model, not as a serialized struct. Anything that wants
// the fields — the journal, a test, a future richer surface — can type-assert
// instead of parsing them back out of prose.
type CallError struct {
	Tool    string
	Message string
	Detail  *ErrorDetail
}

func (e *CallError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool %q: %s", e.Tool, e.Message)
	if e.Detail == nil {
		return b.String()
	}

	// Parenthetical, comma-separated, and only for fields that were actually set.
	// A tool that reports nothing reads exactly as it did before this type
	// existed, which is what keeps the common case uncluttered.
	var parts []string
	if e.Detail.Code != "" {
		parts = append(parts, "code "+e.Detail.Code)
	}
	if e.Detail.Name != "" && e.Detail.Name != "Error" {
		// "Error" is the default class and says nothing; naming it would be noise
		// on every ordinary throw.
		parts = append(parts, e.Detail.Name)
	}
	if e.Detail.Retryable != nil {
		if *e.Detail.Retryable {
			parts = append(parts, "retryable")
		} else {
			parts = append(parts, "not retryable")
		}
	}
	if len(parts) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(parts, ", "))
	}
	if len(e.Detail.Cause) > 0 {
		fmt.Fprintf(&b, "; caused by: %s", strings.Join(e.Detail.Cause, ": "))
	}
	return b.String()
}

// Retryable reports whether the tool said trying again could work. The second
// return distinguishes "said no" from "did not say", which the caller needs
// because the two justify different behavior: one is a decision, the other is an
// absence of one.
func (e *CallError) Retryable() (retryable, stated bool) {
	if e.Detail == nil || e.Detail.Retryable == nil {
		return false, false
	}
	return *e.Detail.Retryable, true
}

// Code returns the tool's own failure code, or "".
func (e *CallError) Code() string {
	if e.Detail == nil {
		return ""
	}
	return e.Detail.Code
}
