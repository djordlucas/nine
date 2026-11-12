package cli

import (
	"fmt"
	"strings"

	"nine/internal/config"
	"nine/internal/protocol"
)

// Board prints the board: the shared task queue (adr/orchestrator-board.md).
func (c *CLI) Board(cfg *config.Config) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	tasks, err := cl.BoardList("")
	if err != nil {
		return fmt.Errorf("list board: %w", err)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(c.Out, "The board is empty.")
		return nil
	}
	idW, titleW := len("ID"), len("TITLE")
	for _, t := range tasks {
		idW, titleW = max(idW, len(t.ID)), max(titleW, len(oneLine(t.Title)))
	}
	fmt.Fprintf(c.Out, "%-*s %-*s %-9s %-10s %s\n", idW, "ID", titleW, "TITLE", "STATUS", "POSTED BY", "CLAIMED BY")
	for _, t := range tasks {
		fmt.Fprintf(c.Out, "%-*s %-*s %-9s %-10s %s\n",
			idW, t.ID, titleW, oneLine(t.Title), t.Status, t.PostedBy, orDash(t.ClaimedBy))
	}
	return nil
}

// BoardShow prints one task in full, result included.
func (c *CLI) BoardShow(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	t, err := cl.BoardGet(id)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "id:         %s\n", t.ID)
	fmt.Fprintf(c.Out, "title:      %s\n", t.Title)
	fmt.Fprintf(c.Out, "status:     %s\n", t.Status)
	fmt.Fprintf(c.Out, "posted by:  %s (source %s, depth %d)\n", t.PostedBy, t.Source, t.Depth)
	fmt.Fprintf(c.Out, "claimed by: %s\n", orDash(t.ClaimedBy))
	fmt.Fprintf(c.Out, "created:    %s\n", t.CreatedAt)
	if t.ClaimedAt != "" {
		fmt.Fprintf(c.Out, "claimed:    %s\n", t.ClaimedAt)
	}
	if t.FinishedAt != "" {
		fmt.Fprintf(c.Out, "finished:   %s\n", t.FinishedAt)
	}
	if t.Spec != "" {
		fmt.Fprintf(c.Out, "spec:\n%s\n", t.Spec)
	}
	if t.Result != "" {
		fmt.Fprintf(c.Out, "result:\n%s\n", t.Result)
	}
	return nil
}

// BoardCreate posts a task as the operator.
func (c *CLI) BoardCreate(cfg *config.Config, title, spec string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	t, err := cl.BoardCreate(title, spec, 0)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "posted %s (%s)\n", t.ID, t.Status)
	return nil
}

// BoardEdit patches one task as the operator. Flags: --title --spec --status
// --result --clear-claim.
func (c *CLI) BoardEdit(cfg *config.Config, id string, flags map[string]string, clearClaim bool) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	patch := map[string]any{}
	for k, v := range flags {
		patch[k] = v
	}
	if clearClaim {
		patch["clear_claim"] = true
	}
	if len(patch) == 0 {
		return fmt.Errorf("nothing to edit: pass --title/--spec/--status/--result/--clear-claim")
	}
	t, err := cl.BoardUpdate(id, patch)
	if err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "updated %s (%s)\n", t.ID, t.Status)
	return nil
}

// BoardDelete removes one task as the operator.
func (c *CLI) BoardDelete(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	if err := cl.BoardDelete(id); err != nil {
		return err
	}
	fmt.Fprintf(c.Out, "deleted %s\n", id)
	return nil
}

func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

var _ = protocol.BoardTask{} // keep the protocol import if unused after edits
