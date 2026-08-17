package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }

// The rendering is the whole interface: the dispatcher hands the model an error's
// message and nothing else, so what Error() produces IS what the model reads.
func TestCallErrorRendering(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *CallError
		want string
	}{{
		name: "no detail reads exactly as before",
		err:  &CallError{Tool: "csv_stats", Message: "csv is empty"},
		want: `tool "csv_stats": csv is empty`,
	}, {
		name: "code and retryable",
		err: &CallError{Tool: "weather", Message: "upstream timed out", Detail: &ErrorDetail{
			Code: "E_UPSTREAM", Retryable: boolp(true)}},
		want: `tool "weather": upstream timed out (code E_UPSTREAM, retryable)`,
	}, {
		name: "not retryable is stated, not omitted",
		err: &CallError{Tool: "parse", Message: "date is not ISO-8601", Detail: &ErrorDetail{
			Code: "E_ARGS", Retryable: boolp(false)}},
		want: `tool "parse": date is not ISO-8601 (code E_ARGS, not retryable)`,
	}, {
		name: "plain Error class is not worth naming",
		err: &CallError{Tool: "t", Message: "boom", Detail: &ErrorDetail{
			Name: "Error"}},
		want: `tool "t": boom`,
	}, {
		name: "a real class is",
		err: &CallError{Tool: "t", Message: "out of range", Detail: &ErrorDetail{
			Name: "RangeError"}},
		want: `tool "t": out of range (RangeError)`,
	}, {
		name: "cause chain",
		err: &CallError{Tool: "t", Message: "could not load", Detail: &ErrorDetail{
			Cause: []string{"parse failed", "unexpected token"}}},
		want: `tool "t": could not load; caused by: parse failed: unexpected token`,
	}, {
		name: "everything at once",
		err: &CallError{Tool: "sync", Message: "sync failed", Detail: &ErrorDetail{
			Name: "TypeError", Code: "E_SYNC", Retryable: boolp(false),
			Cause: []string{"row 3 invalid"}}},
		want: `tool "sync": sync failed (code E_SYNC, TypeError, not retryable); caused by: row 3 invalid`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Errorf("Error() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// Unset must be distinguishable from false: one is the tool declining to say,
// the other is the tool saying no, and they justify different behavior.
func TestCallErrorRetryableTristate(t *testing.T) {
	for _, tc := range []struct {
		name             string
		detail           *ErrorDetail
		wantRetry, wantS bool
	}{
		{"no detail", nil, false, false},
		{"detail without retryable", &ErrorDetail{Code: "X"}, false, false},
		{"explicitly false", &ErrorDetail{Retryable: boolp(false)}, false, true},
		{"explicitly true", &ErrorDetail{Retryable: boolp(true)}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &CallError{Tool: "t", Message: "m", Detail: tc.detail}
			retry, stated := e.Retryable()
			if retry != tc.wantRetry || stated != tc.wantS {
				t.Errorf("Retryable() = (%v, %v), want (%v, %v)", retry, stated, tc.wantRetry, tc.wantS)
			}
		})
	}
}

// A caller wanting the structure must not have to parse it back out of the prose.
func TestCallErrorIsInspectable(t *testing.T) {
	var err error = &CallError{Tool: "t", Message: "m", Detail: &ErrorDetail{Code: "E_X"}}
	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatal("errors.As did not match *CallError")
	}
	if ce.Code() != "E_X" {
		t.Errorf("Code() = %q, want E_X", ce.Code())
	}
}

// ── the guest side: what a js tool's throw actually produces ────────────────

// callErr runs a tool whose body is `expr` and returns the *CallError.
func callErr(t *testing.T, body string) *CallError {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "boom", `
name = "boom"
kind = "js"
entrypoint = "./boom.js"
description = "boom"
`, body)
	h := openHost(t, dir, nil)
	_, err := h.Call(context.Background(), "boom", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	var ce *CallError
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, not *CallError: %v", err, err)
	}
	return ce
}

func TestJSThrowCarriesStructure(t *testing.T) {
	t.Run("code and retryable are preserved", func(t *testing.T) {
		ce := callErr(t, `export default () => {
  const e = new Error("weather API is down");
  e.code = "E_UPSTREAM";
  e.retryable = true;
  throw e;
};`)
		if ce.Code() != "E_UPSTREAM" {
			t.Errorf("Code() = %q, want E_UPSTREAM", ce.Code())
		}
		if retry, stated := ce.Retryable(); !retry || !stated {
			t.Errorf("Retryable() = (%v, %v), want (true, true)", retry, stated)
		}
	})

	t.Run("cause chain is preserved", func(t *testing.T) {
		ce := callErr(t, `export default () => {
  throw new Error("outer", { cause: new Error("middle", { cause: new Error("inner") }) });
};`)
		if ce.Detail == nil {
			t.Fatal("no detail")
		}
		want := []string{"middle", "inner"}
		if len(ce.Detail.Cause) != len(want) {
			t.Fatalf("Cause = %v, want %v", ce.Detail.Cause, want)
		}
		for i := range want {
			if ce.Detail.Cause[i] != want[i] {
				t.Errorf("Cause[%d] = %q, want %q", i, ce.Detail.Cause[i], want[i])
			}
		}
	})

	t.Run("error class is preserved", func(t *testing.T) {
		ce := callErr(t, `export default () => { throw new RangeError("too big"); };`)
		if ce.Detail == nil || ce.Detail.Name != "RangeError" {
			t.Errorf("Name = %+v, want RangeError", ce.Detail)
		}
	})

	t.Run("a plain throw stays plain", func(t *testing.T) {
		ce := callErr(t, `export default () => { throw new Error("just this"); };`)
		if ce.Message != "just this" {
			t.Errorf("Message = %q", ce.Message)
		}
		// name=Error is captured but deliberately not rendered, so the message a
		// model reads is unchanged from before this feature existed.
		if got := ce.Error(); got != `tool "boom": just this` {
			t.Errorf("Error() = %q, want unchanged rendering", got)
		}
	})

	t.Run("a thrown string does not invent structure", func(t *testing.T) {
		ce := callErr(t, `export default () => { throw "bare string"; };`)
		if ce.Detail != nil {
			t.Errorf("Detail = %+v, want nil for a non-object throw", ce.Detail)
		}
	})

	t.Run("a self-referential cause terminates", func(t *testing.T) {
		// A cycle here would otherwise spin until the wall clock killed the call.
		ce := callErr(t, `export default () => {
  const a = new Error("a"); const b = new Error("b");
  a.cause = b; b.cause = a;
  throw a;
};`)
		if ce.Detail == nil {
			t.Fatal("no detail")
		}
		if len(ce.Detail.Cause) > 8 {
			t.Errorf("cause chain not bounded: %d entries", len(ce.Detail.Cause))
		}
	})
}

// The memory cap should name itself. A bare "out of memory" leaves an author with
// no idea which limit they hit or where it is configured.
func TestOOMNamesTheLimit(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "big", `
name = "big"
kind = "js"
entrypoint = "./big.js"
description = "big"
`, `export default ({ n }) => "x".repeat(n);`)
	h := openHost(t, dir, nil)

	_, err := h.Call(context.Background(), "big", json.RawMessage(`{"n":8388608}`))
	if err == nil {
		t.Skip("8 MiB no longer exhausts the default cap; adjust the test")
	}
	msg := err.Error()
	for _, want := range []string{"out of memory", "memory_mb", "16 MiB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message does not mention %q:\n  %s", want, msg)
		}
	}
}

// …and an ordinary failure must not be decorated with memory advice.
func TestNonOOMErrorIsNotDecorated(t *testing.T) {
	ce := callErr(t, `export default () => { throw new Error("ordinary failure"); };`)
	if strings.Contains(ce.Error(), "memory_mb") {
		t.Errorf("ordinary error carries memory advice: %s", ce.Error())
	}
}
