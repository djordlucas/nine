package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeJobTools struct {
	waitHandle  string
	waitTimeout time.Duration
	listCalled  bool
	cancelH     string
	checkH      string
}

func (f *fakeJobTools) Wait(_ context.Context, h string, d time.Duration) (string, error) {
	f.waitHandle, f.waitTimeout = h, d
	return "waited " + h, nil
}
func (f *fakeJobTools) Check(_ context.Context, h string) (string, error) {
	f.checkH = h
	return "checked " + h, nil
}
func (f *fakeJobTools) List(context.Context) (string, error) {
	f.listCalled = true
	return "listed", nil
}
func (f *fakeJobTools) Cancel(_ context.Context, h string) (string, error) {
	f.cancelH = h
	return "cancelled " + h, nil
}

func dispatchJob(t *testing.T, d *Dispatcher, tool, args string) string {
	t.Helper()
	res, err := d.Dispatch(context.Background(), tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res.Output
}

func TestJobToolsDispatch(t *testing.T) {
	d := New()
	f := &fakeJobTools{}
	RegisterJobTools(d, f)

	// job_wait: default timeout when none is given.
	if out := dispatchJob(t, d, "job_wait", `{"handle":"job_1"}`); out != "waited job_1" {
		t.Errorf("job_wait output = %q", out)
	}
	if f.waitHandle != "job_1" || f.waitTimeout != defaultJobWaitSeconds*time.Second {
		t.Errorf("wait got handle=%q timeout=%s, want job_1 / %ds", f.waitHandle, f.waitTimeout, defaultJobWaitSeconds)
	}

	// job_wait: explicit timeout, clamped to the max.
	dispatchJob(t, d, "job_wait", `{"handle":"j","timeout_seconds":99999}`)
	if f.waitTimeout != maxJobWaitSeconds*time.Second {
		t.Errorf("wait timeout = %s, want clamp to %ds", f.waitTimeout, maxJobWaitSeconds)
	}

	if out := dispatchJob(t, d, "job_check", `{"handle":"job_2"}`); out != "checked job_2" || f.checkH != "job_2" {
		t.Errorf("job_check output=%q handle=%q", out, f.checkH)
	}
	if out := dispatchJob(t, d, "job_list", `{}`); out != "listed" || !f.listCalled {
		t.Errorf("job_list output=%q called=%v", out, f.listCalled)
	}
	if out := dispatchJob(t, d, "job_cancel", `{"handle":"job_3"}`); out != "cancelled job_3" || f.cancelH != "job_3" {
		t.Errorf("job_cancel output=%q handle=%q", out, f.cancelH)
	}
}

func TestJobToolsRequireHandle(t *testing.T) {
	d := New()
	RegisterJobTools(d, &fakeJobTools{})
	for _, tool := range []string{"job_wait", "job_check", "job_cancel"} {
		if _, err := d.Dispatch(context.Background(), tool, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s with no handle should error", tool)
		}
	}
}

func TestJobToolDefsInInterceptedSet(t *testing.T) {
	for _, name := range JobToolNames {
		found := false
		for _, def := range InterceptedDefs {
			if def.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from InterceptedDefs", name)
		}
	}
}
