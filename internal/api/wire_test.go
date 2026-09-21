package api

import (
	"testing"
	"time"
)

// =============================================================================
// Daemon List Envelope Decoding
// =============================================================================

// The daemon wraps list rows in a single-key object. Decoding straight into the
// public response type worked only while the list was empty — an empty reply is
// a bare `[]`, which parses as an array — so these endpoints looked correct
// right up until they had something to return.
func TestDecodeList_Envelope(t *testing.T) {
	raw := `{"goals":[{"id":"g1","description":"ship it","status":"active"}]}`

	rows, err := decodeList[wireGoal](raw, "goals")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Expected 1 row, got %d", len(rows))
	}
	if rows[0].ID != "g1" || rows[0].Description != "ship it" {
		t.Errorf("Unexpected row: %+v", rows[0])
	}
}

// The daemon falls back to a bare array for goals and workflows when it has no
// store, so both shapes have to decode.
func TestDecodeList_BareArray(t *testing.T) {
	rows, err := decodeList[wireGoal](`[]`, "goals")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("Expected 0 rows, got %d", len(rows))
	}
}

func TestDecodeList_Empty(t *testing.T) {
	rows, err := decodeList[wireGoal]("  ", "goals")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rows != nil {
		t.Errorf("Expected nil rows for an empty payload, got %v", rows)
	}
}

func TestDecodeList_WrongKey(t *testing.T) {
	if _, err := decodeList[wireGoal](`{"workflows":[]}`, "goals"); err == nil {
		t.Error("Expected an error when the envelope key is missing")
	}
}

func TestDecodeList_Malformed(t *testing.T) {
	if _, err := decodeList[wireGoal](`not json`, "goals"); err == nil {
		t.Error("Expected an error for a malformed payload")
	}
}

// =============================================================================
// Row Mapping
// =============================================================================

func TestParseStoredTime(t *testing.T) {
	// The shape every nine store write uses: fixed-width RFC3339 UTC micros.
	got := parseStoredTime("2026-08-04T12:34:56.123456Z")
	want := time.Date(2026, 8, 4, 12, 34, 56, 123456000, time.UTC)
	if !got.Equal(want) {
		t.Errorf("Expected %v, got %v", want, got)
	}

	// A malformed timestamp yields the zero time rather than failing the list.
	if ts := parseStoredTime("2026-08-04 12:34:56"); !ts.IsZero() {
		t.Errorf("Expected the zero time for a non-RFC3339 value, got %v", ts)
	}
	if ts := parseStoredTime(""); !ts.IsZero() {
		t.Errorf("Expected the zero time for an empty value, got %v", ts)
	}
}

func TestToGoalInfo(t *testing.T) {
	got := toGoalInfo(wireGoal{
		ID:          "g1",
		Description: "ship it",
		Status:      "active",
		ParentID:    "p1",
		ParentType:  "goal",
		Subtree:     []string{"g2"},
		CreatedAt:   "2026-08-04T12:34:56.123456Z",
		UpdatedAt:   "2026-08-05T12:34:56.123456Z",
	})

	if got.ID != "g1" || got.Description != "ship it" || got.Status != "active" {
		t.Errorf("Unexpected mapping: %+v", got)
	}
	if got.ParentID != "p1" || len(got.Subtree) != 1 {
		t.Errorf("Expected parent and subtree to survive mapping: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Errorf("Expected both timestamps parsed, got %v / %v", got.CreatedAt, got.UpdatedAt)
	}
}

// Steps arrive as a list; the API reports the total and how many are finished.
func TestToWorkflowInfo_SummarisesSteps(t *testing.T) {
	got := toWorkflowInfo(wireWorkflow{
		ID:     "w1",
		Name:   "deploy",
		Status: "active",
		Steps: []wireStep{
			{ID: "s1", Status: "done"},
			{ID: "s2", Status: "skipped"},
			{ID: "s3", Status: "running"},
			{ID: "s4", Status: "pending"},
		},
	})

	if got.Steps != 4 {
		t.Errorf("Expected 4 steps, got %d", got.Steps)
	}
	if got.CompletedSteps != 2 {
		t.Errorf("Expected 2 completed (done + skipped), got %d", got.CompletedSteps)
	}
}

func TestToWorkflowInfo_NoSteps(t *testing.T) {
	got := toWorkflowInfo(wireWorkflow{ID: "w1"})
	if got.Steps != 0 || got.CompletedSteps != 0 {
		t.Errorf("Expected 0/0 for a workflow with no steps, got %d/%d", got.Steps, got.CompletedSteps)
	}
}

func TestToNotification(t *testing.T) {
	got := toNotification(wireNotification{
		ID:        "n1",
		AgentID:   "agent-7",
		Message:   "build failed",
		Seen:      true,
		CreatedAt: "2026-08-04T12:34:56.123456Z",
	})

	if got.ID != "n1" || got.AgentID != "agent-7" || got.Message != "build failed" || !got.Seen {
		t.Errorf("Unexpected mapping: %+v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Error("Expected created_at parsed")
	}
}
