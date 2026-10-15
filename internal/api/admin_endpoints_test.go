package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/backup"
	"nine/internal/protocol"
)

func TestListReflections(t *testing.T) {
	var asked protocol.Msg
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		asked = m
		at := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
		return textReply(t, protocol.TypeSessionEvents, []protocol.JournalEvent{
			{Type: "turn_start", TS: at, Turn: 1},
			{Type: "turn_end", TS: at, Turn: 1, Payload: json.RawMessage(`{"result":"I keep notes tidy."}`)},
			{Type: "turn_end", TS: at, Turn: 2, Payload: json.RawMessage(`{"error":"timeout"}`)},
			{Type: "turn_end", TS: at, Turn: 3, Payload: json.RawMessage(`{"result":""}`)},
		})
	})
	w := doAt(t, sock, "GET", "/api/v1/reflections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if asked.AgentID != selfReflectionAgent {
		t.Errorf("asked for %q, want the self-reflection session", asked.AgentID)
	}
	var got struct {
		Agent string `json:"agent"`
		Data  []struct {
			Text string `json:"text"`
			Turn int    `json:"turn"`
		} `json:"data"`
	}
	decodeJSON(t, w, &got)
	if got.Agent != selfReflectionAgent || len(got.Data) != 1 || got.Data[0].Text != "I keep notes tidy." {
		t.Errorf("response = %+v; want only the turn that concluded something", got)
	}

	doAt(t, sock, "GET", "/api/v1/reflections?agent=sec-watch", "")
	if asked.AgentID != "sec-watch" {
		t.Errorf("asked for %q, want sec-watch", asked.AgentID)
	}
}

func TestSessionsRosterAndDelete(t *testing.T) {
	var deleted string
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeSessionsList:
			return textReply(t, protocol.TypeSessionsList, []protocol.SessionInfo{
				{ID: "abc123", Status: "idle", AgeSeconds: 60, Events: 12},
				{ID: "goal:g1", Status: "running", Protected: true},
			})
		case protocol.TypeSessionDelete:
			deleted = m.AgentID
			return []protocol.Msg{protocol.NewTextMsg(protocol.TypeSessionDelete, "deleted "+m.AgentID+" — 12 rows")}
		}
		return errReply("unexpected " + string(m.Type))
	})

	w := doAt(t, sock, "GET", "/api/v1/sessions", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "goal:g1") {
		t.Fatalf("roster: %d %s", w.Code, w.Body.String())
	}
	if w := doAt(t, sock, "GET", "/api/v1/sessions/abc123", ""); w.Code != http.StatusOK {
		t.Errorf("one session: status %d", w.Code)
	}

	// A prefix the daemon would resolve is not the session's id over HTTP.
	if w := doAt(t, sock, "DELETE", "/api/v1/sessions/abc", ""); w.Code != http.StatusNotFound || deleted != "" {
		t.Errorf("prefix delete: status %d, deleted %q; want 404 and nothing deleted", w.Code, deleted)
	}
	w = doAt(t, sock, "DELETE", "/api/v1/sessions/abc123", "")
	if w.Code != http.StatusOK || deleted != "abc123" || !strings.Contains(w.Body.String(), "12 rows") {
		t.Errorf("delete: %d %s, deleted %q", w.Code, w.Body.String(), deleted)
	}
}

func TestBackupStreamsTheSnapshotAndRemovesIt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var path string
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		if m.Text == protocol.BackupWorkspace {
			return errReply("backup: no workspace is configured ([workspace].root); back up the store alone")
		}
		dir, err := backup.NewTempDir()
		if err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(dir, backup.DBEntry)
		if err := os.WriteFile(path, []byte("SQLite format 3"), 0o600); err != nil {
			t.Fatal(err)
		}
		return textReply(t, protocol.TypeBackup, protocol.BackupResult{Path: path, Name: "nine-20261015-090000.db", Bytes: 15})
	})

	w := doAt(t, sock, "GET", "/api/v1/backup", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != "SQLite format 3" {
		t.Errorf("body = %q", w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="nine-20261015-090000.db"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the snapshot's directory outlived the download: %v", err)
	}

	if w := doAt(t, sock, "GET", "/api/v1/backup?with=workspace", ""); w.Code != http.StatusBadRequest {
		t.Errorf("no workspace: status %d, want 400", w.Code)
	}
}
