package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitState polls until job id reaches want, failing on a wrong terminal state
// or a timeout.
func waitState(t *testing.T, j *Jobs, id string, want JobState) JobStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st := j.status(id)
		if st.State == want {
			return st
		}
		if st.State.terminal() && st.State != want {
			t.Fatalf("job %s reached %s, want %s (err=%q)", id, st.State, want, st.Error)
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s in time (last=%s)", id, want, j.status(id).State)
	return JobStatus{}
}

func TestJobDoneCarriesOutput(t *testing.T) {
	j := NewJobs()
	id := j.start(Job{Ack: "started", Run: func(context.Context) (string, error) {
		return "the result", nil
	}})
	st := waitState(t, j, id, JobDone)
	if st.Output != "the result" {
		t.Errorf("output = %q, want %q", st.Output, "the result")
	}
}

func TestJobFailureCarriesError(t *testing.T) {
	j := NewJobs()
	id := j.start(Job{Run: func(context.Context) (string, error) {
		return "", context.DeadlineExceeded
	}})
	st := waitState(t, j, id, JobFailed)
	if st.Error == "" {
		t.Error("failed job should carry an error message")
	}
}

func TestJobCancelWhileRunning(t *testing.T) {
	j := NewJobs()
	started := make(chan struct{})
	id := j.start(Job{Run: func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}})
	<-started
	waitState(t, j, id, JobRunning)
	if !j.cancel(id) {
		t.Fatal("cancel of a known job should return true")
	}
	waitState(t, j, id, JobCancelled)
}

func TestJobUnknownID(t *testing.T) {
	j := NewJobs()
	st := j.status("nope")
	if st.State != JobFailed || st.Error == "" {
		t.Errorf("unknown id status = %+v, want failed with an error", st)
	}
	if j.cancel("nope") {
		t.Error("cancel of an unknown id should return false")
	}
}

// max_concurrent bounds running jobs: a second job past the cap sits queued
// until the first frees the slot, and a queued job can be cancelled without ever
// running.
func TestJobCapQueuesAndCancels(t *testing.T) {
	j := NewJobs()
	j.sem = make(chan struct{}, 1) // cap = 1

	releaseA := make(chan struct{})
	idA := j.start(Job{Run: func(context.Context) (string, error) {
		<-releaseA
		return "a", nil
	}})
	waitState(t, j, idA, JobRunning) // A holds the only slot

	ranB := false
	idB := j.start(Job{Run: func(context.Context) (string, error) {
		ranB = true
		return "b", nil
	}})
	if st := j.status(idB); st.State != JobQueued {
		t.Fatalf("second job state = %s, want queued", st.State)
	}

	if !j.cancel(idB) {
		t.Fatal("cancel of queued job should return true")
	}
	waitState(t, j, idB, JobCancelled)
	if ranB {
		t.Error("a cancelled-while-queued job must never run")
	}

	close(releaseA)
	waitState(t, j, idA, JobDone)
}

func TestJobDirCreatedUnderCache(t *testing.T) {
	j := NewJobs()
	j.cache = t.TempDir()

	var gotDir string
	id := j.start(Job{Run: func(ctx context.Context) (string, error) {
		d, err := JobDir(ctx)
		if err != nil {
			return "", err
		}
		gotDir = d
		return "ok", nil
	}})
	waitState(t, j, id, JobDone)

	if gotDir != filepath.Join(j.cache, "jobs", id) {
		t.Errorf("job dir = %q, want %q", gotDir, filepath.Join(j.cache, "jobs", id))
	}
	if fi, err := os.Stat(gotDir); err != nil || !fi.IsDir() {
		t.Errorf("job dir should exist: err=%v", err)
	}
}

func TestSetProgressVisibleInStatus(t *testing.T) {
	j := NewJobs()
	proceed := make(chan struct{})
	id := j.start(Job{Run: func(ctx context.Context) (string, error) {
		SetProgress(ctx, "halfway")
		<-proceed
		return "done", nil
	}})
	// Spin until the progress note lands (set at the top of Run).
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && j.status(id).Progress == "" {
		time.Sleep(2 * time.Millisecond)
	}
	if got := j.status(id).Progress; got != "halfway" {
		t.Errorf("progress = %q, want halfway", got)
	}
	close(proceed)
	waitState(t, j, id, JobDone)
}

func TestJobEvictionDropsTerminal(t *testing.T) {
	j := NewJobs()
	dir := t.TempDir()
	j.cache = dir
	id := j.start(Job{Run: func(ctx context.Context) (string, error) {
		d, _ := JobDir(ctx)
		os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o600) //nolint:errcheck
		return "ok", nil
	}})
	waitState(t, j, id, JobDone)

	// Backdate the finish time past the TTL, then a status call evicts it.
	j.mu.Lock()
	j.jobs[id].finished = time.Now().Add(-2 * j.ttl)
	jobDir := j.jobs[id].dir
	j.mu.Unlock()

	if st := j.status(id); st.State != JobFailed || st.Error == "" {
		t.Errorf("evicted job should read back as unknown (failed), got %+v", st)
	}
	if _, err := os.Stat(jobDir); !os.IsNotExist(err) {
		t.Errorf("evicted job dir should be removed (stat err = %v)", err)
	}
}

// --- handleRPC dispatch (wire wiring) ---

func rpcCall(t *testing.T, method string, params any, handlers map[string]ToolHandler, jobHandlers map[string]JobHandler, jobs *Jobs) (json.RawMessage, *RPCErr) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"method": method, "params": params})
	req := httptest.NewRequest("POST", "/rpc", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	describe := DescribeResult{ProtocolVersion: ProtocolVersion, AsyncJobs: len(jobHandlers) > 0}
	handleRPC(rec, req, describe, handlers, jobHandlers, jobs)

	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCErr         `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	return env.Result, env.Error
}

func TestHandleRPCJobCall(t *testing.T) {
	jobs := NewJobs()
	release := make(chan struct{})
	jh := map[string]JobHandler{
		"dl": func(context.Context, json.RawMessage) (Job, error) {
			return Job{Ack: "started dl", Run: func(context.Context) (string, error) {
				<-release
				return "downloaded", nil
			}}, nil
		},
	}

	res, rpcErr := rpcCall(t, "plugin.call", map[string]any{"tool": "dl"}, nil, jh, jobs)
	if rpcErr != nil {
		t.Fatalf("unexpected rpc error: %v", rpcErr)
	}
	var got struct {
		JobID  string `json:"job_id"`
		Output string `json:"output"`
	}
	json.Unmarshal(res, &got) //nolint:errcheck
	if got.JobID == "" {
		t.Error("job call should return a job_id")
	}
	if got.Output != "started dl" {
		t.Errorf("output = %q, want the ack", got.Output)
	}

	// job_status over the wire reflects the running job, then its result.
	sres, _ := rpcCall(t, "plugin.job_status", map[string]any{"job_id": got.JobID}, nil, jh, jobs)
	var st JobStatus
	json.Unmarshal(sres, &st) //nolint:errcheck
	if st.State != JobRunning && st.State != JobQueued {
		t.Errorf("state = %s, want running/queued", st.State)
	}

	close(release)
	waitState(t, jobs, got.JobID, JobDone)

	// cancel of a finished job still answers cancelled:true (best-effort, known id).
	cres, _ := rpcCall(t, "plugin.job_cancel", map[string]any{"job_id": got.JobID}, nil, jh, jobs)
	var cancelled struct {
		Cancelled bool `json:"cancelled"`
	}
	json.Unmarshal(cres, &cancelled) //nolint:errcheck
	if !cancelled.Cancelled {
		t.Error("cancel of a known job id should answer cancelled:true")
	}
}

func TestHandleRPCJobValidationError(t *testing.T) {
	jh := map[string]JobHandler{
		"dl": func(_ context.Context, args json.RawMessage) (Job, error) {
			return Job{}, InvalidArgs("bad url")
		},
	}
	_, rpcErr := rpcCall(t, "plugin.call", map[string]any{"tool": "dl"}, nil, jh, NewJobs())
	if rpcErr == nil || rpcErr.Code != -32602 {
		t.Errorf("validation error = %v, want an invalid-params RPC error", rpcErr)
	}
}

func TestHandleRPCJobMethodsRequireSupport(t *testing.T) {
	// jobs == nil: the plugin does not support jobs, so the methods are unknown.
	_, rpcErr := rpcCall(t, "plugin.job_status", map[string]any{"job_id": "x"}, nil, nil, nil)
	if rpcErr == nil || rpcErr.Code != -32601 {
		t.Errorf("job_status without support = %v, want method-not-found", rpcErr)
	}
}
