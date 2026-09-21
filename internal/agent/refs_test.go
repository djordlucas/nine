package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nine/internal/llm"
)

// refTool builds a dispatcher with one tool whose schema marks the named
// properties as refs, and a handler that records the arguments it received.
func refTool(t *testing.T, schema string) (*Dispatcher, *json.RawMessage) {
	t.Helper()
	var seen json.RawMessage
	d := New()
	d.declareRefParams("consume", json.RawMessage(schema))
	d.InjectHandler("consume", func(_ context.Context, args json.RawMessage) (string, error) {
		seen = args
		return "ok", nil
	})
	return d, &seen
}

const refSchema = `{"type":"object","properties":{
	"content":{"type":"string","x-nine-ref":true},
	"label":{"type":"string"}}}`

func TestRefParamsFromSchema(t *testing.T) {
	got := refParams(json.RawMessage(refSchema))
	if len(got) != 1 || got[0] != "content" {
		t.Errorf("refParams = %v, want [content]", got)
	}
	if got := refParams(json.RawMessage(`{"properties":{"a":{"type":"string"}}}`)); got != nil {
		t.Errorf("unmarked schema yielded %v, want none", got)
	}
	if got := refParams(json.RawMessage(`not json`)); got != nil {
		t.Errorf("unparseable schema yielded %v; it must never make the dispatcher guess", got)
	}
	if got := refParams(nil); got != nil {
		t.Errorf("empty schema yielded %v", got)
	}
	// A non-bool marker is not an opt-in.
	if got := refParams(json.RawMessage(`{"properties":{"a":{"x-nine-ref":"yes"}}}`)); got != nil {
		t.Errorf("non-boolean marker yielded %v", got)
	}
}

func TestRefExpandedIntoArgs(t *testing.T) {
	d, seen := refTool(t, refSchema)
	d.SetRefResolver(func(_ context.Context, path string) (string, error) {
		if path != "spill/a1/http-01.txt" {
			return "", errors.New("unexpected path " + path)
		}
		return "THE FULL PAYLOAD", nil
	})

	_, err := d.Dispatch(context.Background(), "consume",
		json.RawMessage(`{"content":"spill/a1/http-01.txt","label":"keep me"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	var got struct{ Content, Label string }
	if err := json.Unmarshal(*seen, &got); err != nil {
		t.Fatalf("unmarshal handler args: %v", err)
	}
	if got.Content != "THE FULL PAYLOAD" {
		t.Errorf("content = %q, want the resolved payload", got.Content)
	}
	if got.Label != "keep me" {
		t.Errorf("label = %q, want it untouched", got.Label)
	}
}

// The reason refs are declared rather than inferred: file_fetch's `path`
// argument is genuinely a path, and expanding it would replace the path with
// the file's contents and break the tool.
func TestUnmarkedPathArgumentIsNotExpanded(t *testing.T) {
	d, seen := refTool(t, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		t.Fatal("resolver must not be consulted for an unmarked property")
		return "", nil
	})

	_, err := d.Dispatch(context.Background(), "consume",
		json.RawMessage(`{"path":"spill/a1/http-01.txt"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(string(*seen), "spill/a1/http-01.txt") {
		t.Errorf("handler args = %s, want the literal path preserved", *seen)
	}
}

func TestRefResolutionFailureFailsTheCall(t *testing.T) {
	d, _ := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		return "", errors.New("no stored file")
	})

	_, err := d.Dispatch(context.Background(), "consume",
		json.RawMessage(`{"content":"spill/missing.txt"}`))
	if err == nil {
		t.Fatal("expected an error the model can see and correct")
	}
	if !strings.Contains(err.Error(), "spill/missing.txt") {
		t.Errorf("error %q should name the unresolvable path", err)
	}
}

func TestOversizeRefRejected(t *testing.T) {
	d, _ := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		return strings.Repeat("x", MaxRefBytes+1), nil
	})

	_, err := d.Dispatch(context.Background(), "consume",
		json.RawMessage(`{"content":"spill/huge.txt"}`))
	if err == nil {
		t.Fatal("expected an error for a ref over MaxRefBytes")
	}
	if !strings.Contains(err.Error(), "read_file") {
		t.Errorf("error %q should point at the paging alternative", err)
	}
}

func TestAbsentOrEmptyRefIsNotMandatory(t *testing.T) {
	d, seen := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		t.Fatal("resolver consulted for an absent/empty ref")
		return "", nil
	})

	for _, args := range []string{`{"label":"no ref here"}`, `{"content":"","label":"x"}`} {
		if _, err := d.Dispatch(context.Background(), "consume", json.RawMessage(args)); err != nil {
			t.Fatalf("dispatch(%s): %v", args, err)
		}
		if string(*seen) != args {
			t.Errorf("args %s were rewritten to %s", args, *seen)
		}
	}
}

func TestNoResolverPassesRefsThrough(t *testing.T) {
	d, seen := refTool(t, refSchema)
	const args = `{"content":"spill/a1/http-01.txt"}`
	if _, err := d.Dispatch(context.Background(), "consume", json.RawMessage(args)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if string(*seen) != args {
		t.Errorf("args = %s, want them untouched with no resolver registered", *seen)
	}
}

// The approval gate and post-call hooks must see the handle the model chose,
// not the megabyte substituted behind it.
func TestApprovalAndHooksSeeOriginalArgs(t *testing.T) {
	d, _ := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		return "THE FULL PAYLOAD", nil
	})

	var approvedArgs, hookArgs string
	d.SetApproval([]string{"consume"}, func(_ context.Context, _ string, args json.RawMessage) error {
		approvedArgs = string(args)
		return nil
	})
	d.AddHook("consume", func(_ string, args json.RawMessage, _ string) {
		hookArgs = string(args)
	})

	const args = `{"content":"spill/a1/http-01.txt"}`
	if _, err := d.Dispatch(context.Background(), "consume", json.RawMessage(args)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if approvedArgs != args {
		t.Errorf("approval saw %s, want the unexpanded %s", approvedArgs, args)
	}
	if hookArgs != args {
		t.Errorf("hook saw %s, want the unexpanded %s", hookArgs, args)
	}
}

func TestRejectedApprovalSkipsRefResolution(t *testing.T) {
	d, _ := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) {
		t.Fatal("a rejected call must not read the ref")
		return "", nil
	})
	d.SetApproval([]string{"consume"}, func(context.Context, string, json.RawMessage) error {
		return errors.New("denied")
	})

	if _, err := d.Dispatch(context.Background(), "consume",
		json.RawMessage(`{"content":"spill/a1/http-01.txt"}`)); err == nil {
		t.Fatal("expected the approval rejection")
	}
}

func TestNonObjectArgsAreLeftAlone(t *testing.T) {
	d, seen := refTool(t, refSchema)
	d.SetRefResolver(func(context.Context, string) (string, error) { return "x", nil })

	if _, err := d.Dispatch(context.Background(), "consume", json.RawMessage(`"a bare string"`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if string(*seen) != `"a bare string"` {
		t.Errorf("args = %s, want them passed through", *seen)
	}
}

// A tool declares its refs in the same schema the model is shown, and the
// dispatcher indexes them when the tool registers.
//
// This used to assert over file_store, a core tool. The ref-taking file tool is
// write_file now, which is sandboxed (adr/file-namespaces.md), so the indexing
// happens at SyncSandboxed rather than at New.
func TestSandboxedRegistrationIndexesRefParams(t *testing.T) {
	d := New()
	d.declareRefParams("write_file", json.RawMessage(
		`{"type":"object","properties":{"path":{"type":"string"},"content_ref":{"type":"string","x-nine-ref":true}}}`))
	if got := d.refParams["write_file"]; len(got) != 1 || got[0] != "content_ref" {
		t.Errorf("write_file ref params = %v, want [content_ref]", got)
	}
}

func TestDeclareToolDefRefParams(t *testing.T) {
	d := New()
	d.declareToolDefRefParams([]llm.ToolDef{{
		Name:        "parse",
		InputSchema: json.RawMessage(refSchema),
	}})
	if got := d.refParams["parse"]; len(got) != 1 || got[0] != "content" {
		t.Errorf("parse ref params = %v, want [content]", got)
	}
}
