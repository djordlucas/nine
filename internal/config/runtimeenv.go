package config

import (
	"os"
	"strings"
)

// Runtime labels describe where the daemon is executing. They reach the model in
// the self-model's Environment block, so they are written for a reader, not for
// a parser.
const (
	RuntimeHost       = "host"
	RuntimeContainer  = "container"
	RuntimeDocker     = "Docker container"
	RuntimePodman     = "Podman container"
	RuntimeKubernetes = "Kubernetes pod"
)

// containerProbes are filesystem markers that identify a container runtime,
// in the order they are checked.
var containerProbes = []struct {
	path  string
	label string
}{
	{"/.dockerenv", RuntimeDocker},        // Docker
	{"/run/.containerenv", RuntimePodman}, // Podman
}

// DetectRuntime reports where this process is running.
//
// It is deliberately conservative about *naming* a runtime. The previous
// implementation probed only `/.dockerenv` and reported "Docker container" or
// "host", which is wrong twice over: Podman and several Kubernetes runtimes do
// not create that file, so a containerized daemon reported itself as running on
// the host. Telling the model it is on the host when it is confined is the
// failure that matters — it invites the model to reason about the operator's
// machine.
//
// So each specific marker is checked first, and a generic cgroup check catches
// the rest: an unrecognized container still reports as a container, just without
// a brand.
//
// Detection happens once, at boot, because the answer cannot change while the
// process runs; the result is carried in config rather than re-probed from
// inside prompt assembly.
func DetectRuntime() string {
	// Kubernetes advertises itself in the environment and is more specific than
	// the container runtime underneath it, so it is checked first.
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return RuntimeKubernetes
	}
	for _, p := range containerProbes {
		if _, err := os.Stat(p.path); err == nil {
			return p.label
		}
	}
	if inContainerCgroup() {
		return RuntimeContainer
	}
	return RuntimeHost
}

// inContainerCgroup reports whether PID 1's cgroup names a container runtime.
//
// This is the catch-all for runtimes that leave no marker file. It reads only
// PID 1 because that is the process whose cgroup identifies the sandbox; a
// missing or unreadable file means "not detectable", never an error — on darwin
// and on a plain host the file simply is not there.
func inContainerCgroup() bool {
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	for _, marker := range []string{"docker", "containerd", "kubepods", "libpod", "lxc"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// RuntimeLabel returns the runtime description for this daemon: the operator's
// `[daemon].runtime` when set, otherwise the detected value.
//
// The override exists because detection is a heuristic and the operator always
// knows better. It is also the only way to describe a sandbox that leaves no
// trace at all.
func (cfg *Config) RuntimeLabel() string {
	if r := strings.TrimSpace(cfg.Daemon.Runtime); r != "" {
		return r
	}
	return DetectRuntime()
}
