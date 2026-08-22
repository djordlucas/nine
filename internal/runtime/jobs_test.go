package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/plugin"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func buildPlugin(t *testing.T, pkg string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), filepath.Base(pkg))
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Dir = moduleRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return out
}

// End to end: a plugin job started through the JobStarter is polled by the
// sweeper to completion, its result recorded, and its owner notified.
func TestJobSweeperCompletesAndNotifies(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	bin := buildPlugin(t, "./internal/plugin/slowplugin")
	mgr := plugin.NewManager("")
	p, err := mgr.Start(bin)
	if err != nil {
		t.Fatalf("start slowplugin: %v", err)
	}
	defer mgr.StopAll() //nolint:errcheck
	if !p.AsyncJobs {
		t.Fatal("slowplugin should advertise async_jobs")
	}

	// Start the detached job and record it exactly as the dispatcher would.
	ctx := context.Background()
	cr, err := mgr.Call(ctx, p, "slowjob", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call slowjob: %v", err)
	}
	if cr.JobID == "" {
		t.Fatal("slowjob should return a job id")
	}
	obs, err := newJobStarter(store, mgr, "conv1", 8).StartJob(ctx, p.Name, "slowjob", cr.JobID, cr.Output)
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	handle := extractHandle(t, obs)

	// Drive the sweeper until the job finishes.
	sw := &jobSweeper{store: store, mgr: mgr}
	deadline := time.Now().Add(3 * time.Second)
	var got memory.Job
	for time.Now().Before(deadline) {
		sw.sweepOnce(ctx)
		j, ok, err := store.JobGet(handle)
		if err != nil {
			t.Fatal(err)
		}
		if ok && memory.JobTerminal(j.State) {
			got = j
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got.State != "done" {
		t.Fatalf("job did not complete: state=%q", got.State)
	}
	if got.Output != "job done" {
		t.Errorf("job output = %q, want %q", got.Output, "job done")
	}

	// The owner was notified so the next turn learns of it.
	ns, err := store.NotificationListPending("conv1")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, n := range ns {
		if strings.Contains(n.Message, handle) {
			found = true
		}
	}
	if !found {
		t.Errorf("no completion notification for conv1 mentioning %s; got %+v", handle, ns)
	}
}

func extractHandle(t *testing.T, obs string) string {
	t.Helper()
	i := strings.Index(obs, "job_")
	if i < 0 {
		t.Fatalf("no handle in observation %q", obs)
	}
	h := obs[i:]
	if j := strings.IndexByte(h, ':'); j >= 0 {
		h = h[:j]
	}
	return h
}
