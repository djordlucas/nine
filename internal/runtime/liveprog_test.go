package runtime

import (
	"context"
	"encoding/json"
)

// Test live programs registered in the Go registry (the shipped pursue/reflect
// are covered by the same path): "relay" ports the JS fixture used across the
// live-process tests — turn every trigger into a model turn and report the
// reply; "eventrelay" turns events into turns; "gen" returns without ever
// waiting, which must record a failure.
func init() {
	livePrograms["relay"] = func(ctx context.Context, h ProcessHandler) error {
		for {
			t, err := h.Next(ctx)
			if err != nil {
				return err
			}
			text := t.Text
			if t.Kind == "clock" {
				text = "tick"
			}
			reply, err := h.Turn(ctx, text)
			if err != nil {
				return err
			}
			if _, err := h.Report(ctx, reply); err != nil {
				return err
			}
		}
	}
	livePrograms["eventrelay"] = func(ctx context.Context, h ProcessHandler) error {
		for {
			t, err := h.Next(ctx)
			if err != nil {
				return err
			}
			if t.Kind == "event" {
				if _, err := h.Turn(ctx, string(t.Event)); err != nil {
					return err
				}
			}
		}
	}
	livePrograms["gen"] = func(ctx context.Context, h ProcessHandler) error {
		return nil // returns without ever calling Next: not a process program
	}
	_ = json.Marshal
}
