package config_test

import (
	"testing"

	"nine/internal/config"
)

// An operator's [daemon].runtime is authoritative: detection is a heuristic, and
// this is the only way to describe a sandbox that leaves no trace.
func TestRuntimeLabelPrefersOperatorOverride(t *testing.T) {
	var cfg config.Config
	cfg.Daemon.Runtime = "Firecracker microVM"
	if got := cfg.RuntimeLabel(); got != "Firecracker microVM" {
		t.Errorf("RuntimeLabel() = %q, want the configured override", got)
	}
}

// Whitespace-only is not a value. Treating it as one would report a runtime of
// " " to the model.
func TestRuntimeLabelIgnoresBlankOverride(t *testing.T) {
	var cfg config.Config
	cfg.Daemon.Runtime = "   "
	if got := cfg.RuntimeLabel(); got != config.DetectRuntime() {
		t.Errorf("RuntimeLabel() = %q, want it to fall back to detection", got)
	}
}

func TestRuntimeLabelFallsBackToDetection(t *testing.T) {
	var cfg config.Config
	if got := cfg.RuntimeLabel(); got != config.DetectRuntime() {
		t.Errorf("RuntimeLabel() = %q, want DetectRuntime() = %q", got, config.DetectRuntime())
	}
}

// Detection must always name something. An empty label omits the Environment
// line, which is the right behavior for "unknown" but wrong as a detection
// result — the caller could not tell the two apart.
func TestDetectRuntimeAlwaysReturnsALabel(t *testing.T) {
	got := config.DetectRuntime()
	if got == "" {
		t.Fatal("DetectRuntime() = \"\", want a non-empty label")
	}
	known := map[string]bool{
		config.RuntimeHost: true, config.RuntimeContainer: true,
		config.RuntimeDocker: true, config.RuntimePodman: true,
		config.RuntimeKubernetes: true,
	}
	if !known[got] {
		t.Errorf("DetectRuntime() = %q, want one of the declared labels", got)
	}
}

// Kubernetes is identified by an environment variable and takes precedence over
// the container runtime underneath it, which is the more useful answer.
func TestDetectRuntimeIdentifiesKubernetes(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	if got := config.DetectRuntime(); got != config.RuntimeKubernetes {
		t.Errorf("DetectRuntime() = %q, want %q", got, config.RuntimeKubernetes)
	}
}

// Detection is called once at boot, but it must still be free of side effects —
// two calls in a row cannot disagree.
func TestDetectRuntimeIsStable(t *testing.T) {
	if a, b := config.DetectRuntime(), config.DetectRuntime(); a != b {
		t.Errorf("DetectRuntime() returned %q then %q", a, b)
	}
}
