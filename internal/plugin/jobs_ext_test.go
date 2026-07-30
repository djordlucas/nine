package plugin_test

import (
	"testing"

	"nine/internal/plugin"
)

// A job-capable plugin advertises async_jobs, its job tool, and protocol v2 —
// and Probe (which runs the version check) accepts it.
func TestSlowpluginAdvertisesJobs(t *testing.T) {
	bin := buildBinary(t, "./internal/plugin/slowplugin")

	desc, err := plugin.Probe(bin)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !desc.AsyncJobs {
		t.Error("slowplugin should advertise async_jobs")
	}
	if desc.ProtocolVersion != plugin.ProtocolVersion {
		t.Errorf("protocol version = %d, want %d", desc.ProtocolVersion, plugin.ProtocolVersion)
	}
	var hasJob bool
	for _, tool := range desc.Tools {
		if tool.Name == "slowjob" {
			hasJob = true
		}
	}
	if !hasJob {
		t.Errorf("slowjob tool not advertised; got %d tools", len(desc.Tools))
	}
}
