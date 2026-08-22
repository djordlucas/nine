package agent

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/plugin"
	"nine/internal/toolvm"
)

type fakeJobStarter struct {
	plugin, tool, jobID, ack string
	ret                      string
	err                      error
	called                   bool

	// The tool-backend half.
	toolCalled bool
	toolName   string
	toolArgs   string
	toolCont   *toolvm.Continuation
}

func (f *fakeJobStarter) StartJob(_ context.Context, pluginName, tool, jobID, ack string) (string, error) {
	f.called = true
	f.plugin, f.tool, f.jobID, f.ack = pluginName, tool, jobID, ack
	return f.ret, f.err
}

func (f *fakeJobStarter) StartToolJob(_ context.Context, tool string, args json.RawMessage, c *toolvm.Continuation) (string, error) {
	f.toolCalled = true
	f.toolName, f.toolArgs, f.toolCont = tool, string(args), c
	return f.ret, f.err
}

func TestResolveCallResultPlainOutput(t *testing.T) {
	d := New()
	d.SetJobStarter(&fakeJobStarter{})
	out, err := d.resolveCallResult(context.Background(), &plugin.Plugin{Name: "p"}, "t", plugin.CallResult{Output: "hello"})
	if err != nil || out != "hello" {
		t.Fatalf("plain result: out=%q err=%v, want hello", out, err)
	}
}

func TestResolveCallResultRecordsJob(t *testing.T) {
	d := New()
	fs := &fakeJobStarter{ret: "recorded as job_x"}
	d.SetJobStarter(fs)

	out, err := d.resolveCallResult(context.Background(),
		&plugin.Plugin{Name: "weather", AsyncJobs: true}, "forecast",
		plugin.CallResult{Output: "started forecast", JobID: "j7"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "recorded as job_x" {
		t.Errorf("observation = %q, want the starter's return", out)
	}
	if fs.plugin != "weather" || fs.tool != "forecast" || fs.jobID != "j7" || fs.ack != "started forecast" {
		t.Errorf("starter got %+v, want weather/forecast/j7/started forecast", fs)
	}
}

func TestResolveCallResultFailsClosedForNonAsyncPlugin(t *testing.T) {
	d := New()
	fs := &fakeJobStarter{}
	d.SetJobStarter(fs)

	_, err := d.resolveCallResult(context.Background(),
		&plugin.Plugin{Name: "p", AsyncJobs: false}, "t",
		plugin.CallResult{JobID: "j1"})
	if err == nil {
		t.Fatal("a job id from a plugin that did not advertise async_jobs must be rejected")
	}
	if fs.called {
		t.Error("the job must not be recorded when fail-closed")
	}
}

func TestResolveCallResultErrorsWithoutStarter(t *testing.T) {
	d := New() // no SetJobStarter
	_, err := d.resolveCallResult(context.Background(),
		&plugin.Plugin{Name: "p", AsyncJobs: true}, "t",
		plugin.CallResult{JobID: "j1"})
	if err == nil {
		t.Fatal("a job id with no JobStarter wired must be an error, not silently dropped")
	}
}
