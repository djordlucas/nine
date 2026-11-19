package memory

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Agent export/import bundles (adr/agents-concept.md A3, Q6): a bundle is a tar
// holding one file, agent.json — the manifest with the agent's name, role,
// skills, self-model and reflection config. Phase 2 carries the manifest only;
// phase 3 (A4) adds the agent's database as agent.db in the same tar.

const (
	// AgentBundleFormat identifies the bundle's manifest.
	AgentBundleFormat = "nine-agent"
	// AgentBundleVersion is the manifest schema version.
	AgentBundleVersion = 1
	// AgentManifestFile is the manifest's name inside the tar.
	AgentManifestFile = "agent.json"
	// AgentDBFile is the agent's database inside the tar (phase 3+, Q6).
	AgentDBFile = "agent.db"
)

// AgentManifest is the agent.json inside an export bundle.
type AgentManifest struct {
	Format          string          `json:"format"`
	Version         int             `json:"version"`
	Name            string          `json:"name"`
	Role            string          `json:"role,omitempty"`
	Skills          json.RawMessage `json:"skills,omitempty"`
	SelfModel       json.RawMessage `json:"self_model,omitempty"`
	Reflection      json.RawMessage `json:"reflection,omitempty"`
	SourceAgentID   string          `json:"source_agent_id,omitempty"`
	SourceVersion   int             `json:"source_version,omitempty"`
	ExportedAt      string          `json:"exported_at"`
}

// AgentBundleTar renders an agent row as a bundle tar: the agent.json manifest,
// plus agent.db (the agent's database file) when db is non-empty (Q6 — phase 3
// onward carries the data; earlier phases exported the manifest only).
func AgentBundleTar(a Agent) ([]byte, error) { return AgentBundleTarWithDB(a, nil) }

// AgentBundleTarWithDB is AgentBundleTar with the agent's database bytes
// included as agent.db.
func AgentBundleTarWithDB(a Agent, db []byte) ([]byte, error) {
	manifest := AgentManifest{
		Format:        AgentBundleFormat,
		Version:       AgentBundleVersion,
		Name:          a.Name,
		Role:          a.Role,
		Skills:        json.RawMessage(a.Skills),
		SelfModel:     json.RawMessage(a.SelfModel),
		Reflection:    json.RawMessage(a.Reflection),
		SourceAgentID: a.ID,
		SourceVersion: a.Version,
		ExportedAt:    nowText(),
	}
	if manifest.Skills == nil {
		manifest.Skills = json.RawMessage("[]")
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name: AgentManifestFile, Mode: 0o644, Size: int64(len(raw)), ModTime: time.Now(),
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(raw); err != nil {
		return nil, err
	}
	if len(db) > 0 {
		if err := tw.WriteHeader(&tar.Header{
			Name: AgentDBFile, Mode: 0o600, Size: int64(len(db)), ModTime: time.Now(),
		}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(db); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseAgentBundleTar reads a bundle tar and validates its manifest. It returns
// the manifest; the caller decides what to do with it (phase 2: import creates a
// staged agent from it).
func ParseAgentBundleTar(data []byte) (AgentManifest, error) {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return AgentManifest{}, fmt.Errorf("agent bundle: no %s in tar", AgentManifestFile)
		}
		if err != nil {
			return AgentManifest{}, fmt.Errorf("agent bundle: %w", err)
		}
		if hdr.Name != AgentManifestFile {
			continue // agent.db and anything else is data for the importer, not the manifest
		}
		raw, err := io.ReadAll(tr)
		if err != nil {
			return AgentManifest{}, fmt.Errorf("agent bundle: read manifest: %w", err)
		}
		var m AgentManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return AgentManifest{}, fmt.Errorf("agent bundle: manifest is not JSON: %w", err)
		}
		if m.Format != AgentBundleFormat {
			return AgentManifest{}, fmt.Errorf("agent bundle: format %q, want %q", m.Format, AgentBundleFormat)
		}
		if m.Version != AgentBundleVersion {
			return AgentManifest{}, fmt.Errorf("agent bundle: version %d, want %d", m.Version, AgentBundleVersion)
		}
		if m.Name == "" {
			return AgentManifest{}, fmt.Errorf("agent bundle: manifest has no name")
		}
		return m, nil
	}
}
