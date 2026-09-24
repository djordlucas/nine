package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// genHost opens a host with the generated tier on, for tools written inline.
func genHost(t *testing.T) *Host {
	t.Helper()
	h, err := Open(context.Background(), Config{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.SetAgentConfig(AgentConfig{Enabled: true})
	return h
}

func callSource(t *testing.T, h *Host, source string) error {
	t.Helper()
	h.LoadGenerated(context.Background(), []Generated{{Name: "probe", Source: source}}, nil)
	if h.Get("probe") == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	_, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`))
	return err
}

// The whole point: a failing tool used to return its thrown error and nothing
// else, so everything it printed on the way to failing was lost to the model.
func TestAFailingToolReturnsWhatItPrinted(t *testing.T) {
	err := callSource(t, genHost(t), `
export default () => {
  console.log("rows parsed: 3");
  console.log("header: id,name");
  throw new Error("column 'total' not found");
};`)
	if err == nil {
		t.Fatal("the tool did not fail")
	}
	got := err.Error()
	for _, want := range []string{"column 'total' not found", "printed before failing", "rows parsed: 3", "header: id,name"} {
		if !strings.Contains(got, want) {
			t.Errorf("error is missing %q:\n%s", want, got)
		}
	}
}

// Logs on success are noise the caller did not ask for.
func TestASucceedingToolReturnsNoLogs(t *testing.T) {
	h := genHost(t)
	h.LoadGenerated(context.Background(), []Generated{{Name: "probe", Source: `
export default () => { console.log("chatty"); return "ok"; };`}}, nil)

	out, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "ok" {
		t.Errorf("output = %q, want the tool's own result and nothing else", out)
	}
}

// A tool printing in a loop must not crowd out the error that explains the
// failure, so the buffer keeps the lines nearest the throw.
func TestPrintedOutputIsBounded(t *testing.T) {
	err := callSource(t, genHost(t), `
export default () => {
  for (let i = 0; i < 500; i++) console.log("line " + i);
  throw new Error("done");
};`)
	if err == nil {
		t.Fatal("the tool did not fail")
	}
	got := err.Error()
	if strings.Contains(got, "line 0\n") || strings.Contains(got, "line 100\n") {
		t.Error("kept the oldest lines; the ones nearest the failure are the useful ones")
	}
	if !strings.Contains(got, "line 499") {
		t.Errorf("dropped the last line printed:\n%s", got)
	}
	if n := strings.Count(got, "\n  "); n > maxLoggedLines {
		t.Errorf("kept %d lines, want at most %d", n, maxLoggedLines)
	}
	if len(got) > 4096 {
		t.Errorf("failure message is %d bytes; the buffer is meant to be bounded", len(got))
	}
}

// One enormous line is truncated rather than dropped: a stringified object still
// names the field that was wrong.
func TestAVeryLongLineIsTruncatedNotDropped(t *testing.T) {
	err := callSource(t, genHost(t), `
export default () => {
  console.log("start:" + "x".repeat(40000));
  throw new Error("boom");
};`)
	if err == nil {
		t.Fatal("the tool did not fail")
	}
	got := err.Error()
	if !strings.Contains(got, "start:") {
		t.Errorf("the long line was dropped entirely:\n%.200s", got)
	}
	if len(got) > 4096 {
		t.Errorf("failure message is %d bytes; a long line must be truncated", len(got))
	}
}

// Two calls must not see each other's output — the buffer is per call, like
// every other piece of state in a call.
func TestPrintedOutputDoesNotLeakBetweenCalls(t *testing.T) {
	h := genHost(t)
	h.LoadGenerated(context.Background(), []Generated{{Name: "probe", Source: `
export default (args) => {
  console.log("secret-" + args.tag);
  if (args.tag === "second") throw new Error("boom");
  return "ok";
};`}}, nil)

	if _, err := h.Call(context.Background(), "probe", json.RawMessage(`{"tag":"first"}`)); err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, err := h.Call(context.Background(), "probe", json.RawMessage(`{"tag":"second"}`))
	if err == nil {
		t.Fatal("second call did not fail")
	}
	if strings.Contains(err.Error(), "secret-first") {
		t.Error("the failing call carried output printed by an earlier call")
	}
	if !strings.Contains(err.Error(), "secret-second") {
		t.Errorf("the failing call lost its own output:\n%s", err)
	}
}
