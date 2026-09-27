package memory

import (
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A request arrives pending, is readable, and settles once.
func TestCapabilityRequestLifecycle(t *testing.T) {
	s := openTestStore(t)

	if err := s.CapabilityRequestCreate("r1", "conv1", "fetch_rates",
		"net.http", `{"allow_hosts":["api.example"]}`, "the tool needs the rates API"); err != nil {
		t.Fatal(err)
	}

	pending, err := s.CapabilityRequestList(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	got := pending[0]
	if got.Capability != "net.http" || got.ToolName != "fetch_rates" {
		t.Errorf("request = %+v", got)
	}
	if got.Status != CapabilityRequestPending {
		t.Errorf("status = %q, want pending", got.Status)
	}

	// Approving settles the request and writes the grant it confers, together.
	ok, err := s.CapabilityRequestDecide("r1", CapabilityRequestApproved, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("approving a pending request reported no change")
	}

	grants, err := s.CapabilityGrantList()
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}
	if grants[0].Source != GrantSourceApproved || grants[0].Capability != "net.http" {
		t.Errorf("grant = %+v", grants[0])
	}
	if grants[0].RequestID != "r1" {
		t.Errorf("grant does not record the request it came from: %+v", grants[0])
	}
	if grants[0].Params != `{"allow_hosts":["api.example"]}` {
		t.Errorf("params did not carry to the grant: %q", grants[0].Params)
	}

	// A second decision is a no-op, not a second grant. Without the status guard,
	// an operator running approve twice would widen the ceiling twice.
	ok, err = s.CapabilityRequestDecide("r1", CapabilityRequestApproved, "g2")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a settled request was decided again")
	}
	if grants, _ := s.CapabilityGrantList(); len(grants) != 1 {
		t.Errorf("grants = %d after a repeat approval, want 1", len(grants))
	}
}

// Denying settles the request and confers nothing.
func TestCapabilityRequestDenyGrantsNothing(t *testing.T) {
	s := openTestStore(t)
	if err := s.CapabilityRequestCreate("r1", "conv1", "t", "net.http", "", "why"); err != nil {
		t.Fatal(err)
	}

	ok, err := s.CapabilityRequestDecide("r1", CapabilityRequestDenied, "g1")
	if err != nil || !ok {
		t.Fatalf("decide: ok=%v err=%v", ok, err)
	}

	grants, err := s.CapabilityGrantList()
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Errorf("a denial wrote %d grants, want 0: %+v", len(grants), grants)
	}

	r, found, err := s.CapabilityRequestGet("r1")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if r.Status != CapabilityRequestDenied || r.DecidedAt == "" {
		t.Errorf("request = %+v, want denied with a decision time", r)
	}
}

// An unknown status is refused rather than stored: the column is an enum, and a
// typo reaching it would make a request neither pending nor settled.
func TestCapabilityRequestDecideRejectsUnknownStatus(t *testing.T) {
	s := openTestStore(t)
	if err := s.CapabilityRequestCreate("r1", "conv1", "t", "net.http", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CapabilityRequestDecide("r1", "maybe", "g1"); err == nil {
		t.Error("an unknown status was accepted")
	}
	if r, _, _ := s.CapabilityRequestGet("r1"); r.Status != CapabilityRequestPending {
		t.Errorf("status = %q after a refused decision, want it untouched", r.Status)
	}
}

// Reconciliation is what makes nine.toml both a seed and a change feed: derived
// grants are replaced wholesale, and approved ones survive it.
func TestCapabilityGrantsReconcilePreservesApproved(t *testing.T) {
	s := openTestStore(t)

	// An operator-approved grant.
	if err := s.CapabilityRequestCreate("r1", "conv1", "t", "net.http", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CapabilityRequestDecide("r1", CapabilityRequestApproved, "g-approved"); err != nil {
		t.Fatal(err)
	}

	// First boot: the file declares an env grant, the daemon derives the workspace.
	first := []CapabilityGrant{
		{ID: "g-cfg", Source: GrantSourceConfig, Capability: "env", Params: `["TZ"]`},
		{ID: "g-def", Source: GrantSourceDefault, Capability: "fs.write", Params: `{"guest":"/work"}`},
	}
	if err := s.CapabilityGrantsReconcile(first); err != nil {
		t.Fatal(err)
	}
	if got := sourceCounts(t, s); got["approved"] != 1 || got["config"] != 1 || got["default"] != 1 {
		t.Fatalf("after the first reconcile: %v", got)
	}

	// Second boot: the operator removed the env grant from the file. It must go,
	// and the approved grant must stay — this is the case a seed-once sentinel
	// cannot express.
	if err := s.CapabilityGrantsReconcile(first[1:]); err != nil {
		t.Fatal(err)
	}
	got := sourceCounts(t, s)
	if got["config"] != 0 {
		t.Errorf("a grant the file stopped declaring is still in force: %v", got)
	}
	if got["default"] != 1 {
		t.Errorf("the derived grant did not survive: %v", got)
	}
	if got["approved"] != 1 {
		t.Errorf("the approved grant was reconciled away: %v", got)
	}
}

// Revocation reaches an approved grant and refuses a derived one, which would
// reappear at the next boot anyway.
func TestCapabilityGrantRevoke(t *testing.T) {
	s := openTestStore(t)
	if err := s.CapabilityRequestCreate("r1", "conv1", "t", "net.http", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CapabilityRequestDecide("r1", CapabilityRequestApproved, "g-approved"); err != nil {
		t.Fatal(err)
	}
	if err := s.CapabilityGrantsReconcile([]CapabilityGrant{
		{ID: "g-cfg", Source: GrantSourceConfig, Capability: "env"},
	}); err != nil {
		t.Fatal(err)
	}

	ok, err := s.CapabilityGrantRevoke("g-approved")
	if err != nil || !ok {
		t.Fatalf("revoking an approved grant: ok=%v err=%v", ok, err)
	}

	ok, err = s.CapabilityGrantRevoke("g-cfg")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a config-sourced grant was revoked; that is an edit to nine.toml, not a command")
	}

	ok, err = s.CapabilityGrantRevoke("nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("revoking an unknown id reported a change")
	}
}

func sourceCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	grants, err := s.CapabilityGrantList()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, g := range grants {
		out[g.Source]++
	}
	return out
}
