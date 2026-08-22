package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"nine/internal/config"
	"nine/internal/protocol"
)

// Sessions prints the session roster: every conversation the store holds, its
// age, how much journal it carries, and whether the retention reaper may take
// it. It requires a running daemon — the attached column is live state.
func (c *CLI) Sessions(cfg *config.Config) error {
	sessions, err := c.sessionRoster(cfg)
	if err != nil || sessions == nil {
		return err
	}
	if len(sessions) == 0 {
		fmt.Fprintln(c.Out, "No sessions.")
		return nil
	}

	// Width from the data, not a guess: ids are a mix of operator-chosen names
	// and 36-character UUIDs, and a fixed column silently misaligns the moment a
	// real session id appears.
	idW := len("ID")
	for _, s := range sessions {
		if len(s.ID) > idW {
			idW = len(s.ID)
		}
	}
	fmt.Fprintf(c.Out, "%-*s %-10s %8s %8s  %s\n", idW, "ID", "STATUS", "AGE", "EVENTS", "NOTE")
	for _, s := range sessions {
		note := ""
		switch {
		case s.Attached:
			note = "attached"
		case s.Protected:
			// Worth saying out loud: an operator looking at a 40-day-old session
			// and wondering why retention has not taken it deserves the reason.
			note = "kept (active goal or plan)"
		}
		name := s.Name
		if name != "" {
			note = strings.TrimSpace(name + " " + note)
		}
		fmt.Fprintf(c.Out, "%-*s %-10s %8s %8d  %s\n",
			idW, s.ID, s.Status, humanAgeShort(s.AgeSeconds), s.Events, note)
	}
	return nil
}

// SessionShow prints one session in full.
func (c *CLI) SessionShow(cfg *config.Config, id string) error {
	sessions, err := c.sessionRoster(cfg)
	if err != nil || sessions == nil {
		return err
	}
	for _, s := range sessions {
		if s.ID != id && !strings.HasPrefix(s.ID, id) {
			continue
		}
		fmt.Fprintf(c.Out, "  id            %s\n", s.ID)
		if s.Name != "" {
			fmt.Fprintf(c.Out, "  name          %s\n", s.Name)
		}
		fmt.Fprintf(c.Out, "  status        %s\n", s.Status)
		fmt.Fprintf(c.Out, "  last active   %s ago\n", humanAgeShort(s.AgeSeconds))
		fmt.Fprintf(c.Out, "  journal       %d events\n", s.Events)
		fmt.Fprintf(c.Out, "  attached      %t\n", s.Attached)
		fmt.Fprintf(c.Out, "  retention     %s\n", retentionNote(s.Protected))
		return nil
	}
	return fmt.Errorf("no session matching %q", id)
}

func retentionNote(protected bool) string {
	if protected {
		return "kept — it has an active goal or session plan"
	}
	return "eligible once it passes [daemon] session_retention_days"
}

// SessionDelete erases a session and everything keyed to it.
//
// It asks first unless force is set, and it says what will go rather than
// asking a bare "are you sure?" — the whole risk here is that the operator does
// not realise a transcript and its journal are about to be destroyed. Stopping a
// session (`nine stop`) keeps all of that; this does not.
func (c *CLI) SessionDelete(cfg *config.Config, id string, force bool) error {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running; sessions can only be deleted through it")
		return nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	if !force {
		sessions, err := cl.ListSessions()
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		target, ok := matchSession(sessions, id)
		if !ok {
			return fmt.Errorf("no session matching %q", id)
		}
		ok, err = c.confirmDelete(target)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(c.Out, "Cancelled; nothing was deleted.")
			return nil
		}
		id = target.ID
	}

	msg, err := cl.DeleteSession(id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	fmt.Fprintln(c.Out, msg)
	return nil
}

// matchSession resolves an exact id or an unambiguous prefix.
func matchSession(sessions []protocol.SessionInfo, id string) (protocol.SessionInfo, bool) {
	for _, s := range sessions {
		if s.ID == id {
			return s, true
		}
	}
	var (
		found protocol.SessionInfo
		n     int
	)
	for _, s := range sessions {
		if strings.HasPrefix(s.ID, id) {
			found, n = s, n+1
		}
	}
	return found, n == 1
}

// confirmDelete describes what is about to be destroyed and waits for a yes.
func (c *CLI) confirmDelete(s protocol.SessionInfo) (bool, error) {
	fmt.Fprintf(c.Out, "About to permanently delete session %s", s.ID)
	if s.Name != "" {
		fmt.Fprintf(c.Out, " (%s)", s.Name)
	}
	fmt.Fprintf(c.Out, ":\n  %d journal events, its transcript, notifications, tool state and jobs.\n",
		s.Events)
	if s.Protected {
		// The reaper would never take this one, so an operator deleting it by hand
		// is overriding a protection rather than tidying up.
		fmt.Fprintln(c.Out, "  This session has an active goal or session plan — retention keeps it.")
	}
	if s.Attached {
		fmt.Fprintln(c.Out, "  It is attached right now; it will be stopped first.")
	}
	fmt.Fprintln(c.Out, "  This cannot be undone. `nine stop` ends a session and keeps its history.")
	fmt.Fprint(c.Out, "Type the session id to confirm: ")

	in := c.In
	if in == nil {
		in = os.Stdin
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	return strings.TrimSpace(line) == s.ID, nil
}

func (c *CLI) sessionRoster(cfg *config.Config) ([]protocol.SessionInfo, error) {
	sock := cfg.SocketPath()
	if !protocol.CanConnect(sock) {
		fmt.Fprintln(c.Out, "no daemon running; start one with `nine serve`")
		return nil, nil
	}
	cl, err := protocol.Connect(sock)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer cl.Close() //nolint:errcheck

	sessions, err := cl.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return sessions, nil
}

// humanAgeShort renders an age compactly for a table column.
func humanAgeShort(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fmt.Sprintf("%dd", seconds/86400)
	}
}
