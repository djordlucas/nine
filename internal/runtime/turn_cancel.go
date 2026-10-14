package runtime

import (
	"encoding/json"
	"fmt"

	"nine/internal/protocol"
)

// Outcomes of turn_cancel, in the reply's text.
const (
	// TurnCancelled: a turn was running and has been told to stop.
	TurnCancelled = "cancelled"
	// TurnIdle: the session exists and no turn was running.
	TurnIdle = "idle"
)

// handleTurnCancel cancels the turn agentID is running. Unlike session_stop it
// leaves the session as it was: loaded, with its history and checkpoint. The
// turn's own connection, and every watcher, receive its outcome: an error
// "turn cancelled".
func (d *Daemon) handleTurnCancel(enc *json.Encoder, agentID string) {
	id := d.resolveID(agentID)
	d.mu.RLock()
	w := d.sessions[id]
	d.mu.RUnlock()
	if w != nil {
		outcome := TurnIdle
		if w.cancel() {
			outcome = TurnCancelled
		}
		enc.Encode(protocol.NewTextMsg(protocol.TypeTurnCancel, outcome)) //nolint:errcheck
		return
	}
	// Not loaded, so not running a turn; say whether it exists at all.
	if d.store != nil && d.sessionKnown(id, 0) {
		enc.Encode(protocol.NewTextMsg(protocol.TypeTurnCancel, TurnIdle)) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("session %s not found", agentID))) //nolint:errcheck
}
