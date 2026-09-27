package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// Grant sources. The distinction is what lets nine.toml stay authoritative for
// what it declares while an operator's approval outlives a boot.
const (
	// GrantSourceDefault is derived by the daemon from [workspace].root.
	GrantSourceDefault = "default"
	// GrantSourceConfig is read from [tools.agent.capabilities].
	GrantSourceConfig = "config"
	// GrantSourceApproved was conferred by an operator answering a request.
	GrantSourceApproved = "approved"
)

// Capability request statuses.
const (
	CapabilityRequestPending  = "pending"
	CapabilityRequestApproved = "approved"
	CapabilityRequestDenied   = "denied"
)

// CapabilityRequest is one row of capability_requests: the agent asking an
// operator to widen the generated tier's ceiling. It is a record of the asking,
// never a grant — approval writes a CapabilityGrant, and that is what confers.
type CapabilityRequest struct {
	ID         string `json:"id"`
	AgentID    string `json:"agent_id"`
	ToolName   string `json:"tool_name,omitempty"`
	Capability string `json:"capability"`
	Params     string `json:"params,omitempty"` // JSON; capability-specific
	Reason     string `json:"reason,omitempty"`
	Status     string `json:"status"` // pending | approved | denied
	CreatedAt  string `json:"created_at"`
	DecidedAt  string `json:"decided_at,omitempty"`
}

// CapabilityGrant is one row of capability_grants: a capability that is in force
// for the generated tier, and where it came from.
type CapabilityGrant struct {
	ID         string `json:"id"`
	Source     string `json:"source"` // default | config | approved
	Capability string `json:"capability"`
	Params     string `json:"params,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// CapabilityRequestCreate records a pending request. This is the only write on
// either table the agent can reach, and it cannot set a status: a request arrives
// pending or not at all.
func (s *Store) CapabilityRequestCreate(id, agentID, toolName, capability, params, reason string) error {
	_, err := s.db.Exec(
		`INSERT INTO capability_requests(id, agent_id, tool_name, capability, params, reason, status, created_at)
		 VALUES(?,?,?,?,?,?,'pending',?)`,
		id, agentID, toolName, capability, nullIfEmpty(params), reason,
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// CapabilityRequestList returns requests newest first. pendingOnly narrows to
// those still awaiting a decision.
func (s *Store) CapabilityRequestList(pendingOnly bool) ([]CapabilityRequest, error) {
	q := `SELECT id, agent_id, tool_name, capability, params, reason, status, created_at, decided_at
	      FROM capability_requests`
	if pendingOnly {
		q += ` WHERE status = 'pending'`
	}
	q += ` ORDER BY created_at DESC, id DESC`

	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []CapabilityRequest
	for rows.Next() {
		r, err := scanCapabilityRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CapabilityRequestGet returns one request, or ok=false when no such id exists.
func (s *Store) CapabilityRequestGet(id string) (CapabilityRequest, bool, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, tool_name, capability, params, reason, status, created_at, decided_at
		 FROM capability_requests WHERE id = ?`, id)
	if err != nil {
		return CapabilityRequest{}, false, err
	}
	defer rows.Close() //nolint:errcheck

	if !rows.Next() {
		return CapabilityRequest{}, false, rows.Err()
	}
	r, err := scanCapabilityRequest(rows)
	if err != nil {
		return CapabilityRequest{}, false, err
	}
	return r, true, nil
}

// CapabilityRequestDecide settles a request and, when approving, writes the grant
// it confers — both in one transaction, so a decision never leaves a request
// marked approved with no grant behind it.
//
// The guard on status is what makes a second approval a no-op rather than a second
// grant: ok=false means the request was already decided, or never existed.
func (s *Store) CapabilityRequestDecide(id, status, grantID string) (ok bool, err error) {
	if status != CapabilityRequestApproved && status != CapabilityRequestDenied {
		return false, fmt.Errorf("capability request status %q must be %q or %q",
			status, CapabilityRequestApproved, CapabilityRequestDenied)
	}

	tx, err := s.db.BeginWrite()
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.Exec(
		`UPDATE capability_requests SET status = ?, decided_at = ?
		 WHERE id = ? AND status = 'pending'`, status, now, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}

	if status == CapabilityRequestApproved {
		var capability, params string
		var p sql.NullString
		if err := tx.QueryRow(
			`SELECT capability, params FROM capability_requests WHERE id = ?`, id,
		).Scan(&capability, &p); err != nil {
			return false, err
		}
		params = p.String
		if _, err := tx.Exec(
			`INSERT INTO capability_grants(id, source, capability, params, request_id, created_at)
			 VALUES(?,?,?,?,?,?)`,
			grantID, GrantSourceApproved, capability, nullIfEmpty(params), id, now,
		); err != nil {
			return false, err
		}
	}

	return true, tx.Commit()
}

// CapabilityGrantList returns every grant in force, approved ones first so an
// operator reading it sees what they conferred before what the file did.
func (s *Store) CapabilityGrantList() ([]CapabilityGrant, error) {
	rows, err := s.db.Query(
		`SELECT id, source, capability, params, request_id, created_at
		 FROM capability_grants
		 ORDER BY CASE source WHEN 'approved' THEN 0 WHEN 'config' THEN 1 ELSE 2 END,
		          capability, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []CapabilityGrant
	for rows.Next() {
		var g CapabilityGrant
		var params, reqID sql.NullString
		if err := rows.Scan(&g.ID, &g.Source, &g.Capability, &params, &reqID, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.Params, g.RequestID = params.String, reqID.String
		out = append(out, g)
	}
	return out, rows.Err()
}

// CapabilityGrantRevoke removes one grant. ok=false means no such id.
//
// Only an `approved` grant is revocable here. A `config` or `default` row is
// derived — it would reappear at the next boot — so revoking one is an edit to
// nine.toml, not a command.
func (s *Store) CapabilityGrantRevoke(id string) (ok bool, err error) {
	res, err := s.db.Exec(
		`DELETE FROM capability_grants WHERE id = ? AND source = 'approved'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CapabilityGrantsReconcile replaces every derived grant with the ones passed,
// in one transaction, and leaves `approved` rows untouched.
//
// Delete-and-reinsert rather than a diff is what makes nine.toml behave as both a
// first-boot seed and a change feed without a sentinel or a stored snapshot: a
// grant the file adds appears, one it edits changes, one it stops declaring goes
// away, and one it never mentioned is not its business. The alternative — seeding
// once behind a sentinel — cannot express the removal, and for a capability
// ceiling removal is the case that has to work.
func (s *Store) CapabilityGrantsReconcile(derived []CapabilityGrant) error {
	tx, err := s.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.Exec(
		`DELETE FROM capability_grants WHERE source IN ('config', 'default')`); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, g := range derived {
		if _, err := tx.Exec(
			`INSERT INTO capability_grants(id, source, capability, params, created_at)
			 VALUES(?,?,?,?,?)`,
			g.ID, g.Source, g.Capability, nullIfEmpty(g.Params), now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanCapabilityRequest(rows *sql.Rows) (CapabilityRequest, error) {
	var r CapabilityRequest
	var params, decided sql.NullString
	if err := rows.Scan(&r.ID, &r.AgentID, &r.ToolName, &r.Capability,
		&params, &r.Reason, &r.Status, &r.CreatedAt, &decided); err != nil {
		return CapabilityRequest{}, err
	}
	r.Params, r.DecidedAt = params.String, decided.String
	return r, nil
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
