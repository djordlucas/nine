package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/config"
	"nine/internal/protocol"
)

// The operator's side of the capability loop (docs/sandboxed-tools.md).
//
// A generated tool that declares a capability the ceiling does not permit is
// refused, and the agent's route out is `capability_request` — a pending row a
// human decides on. These commands are that decision.
//
// They require a running daemon and deliberately do not start one: approving a
// capability should apply to the daemon that asked, and a grant approved against a
// daemon spawned for the purpose would take effect in a process that then exits.

// Grants lists capability requests and the grants in force.
func (c *CLI) Grants(cfg *config.Config, all bool) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	state, err := cl.ListCapabilities()
	if err != nil {
		return err
	}
	printCapabilityState(c.Out, state, all)
	return nil
}

// GrantsDecide settles one request, or revokes one grant.
func (c *CLI) GrantsDecide(cfg *config.Config, action, id string) error {
	if id == "" {
		return fmt.Errorf("usage: nine grants %s <id>", action)
	}
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	text, err := cl.DecideCapability(id, action)
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, text)
	return nil
}

func printCapabilityState(w interface{ Write([]byte) (int, error) }, state protocol.CapabilityState, all bool) {
	pending := 0
	for _, r := range state.Requests {
		if r.Status == "pending" {
			pending++
		}
	}

	if pending == 0 {
		fmt.Fprintln(w, "No capability requests are waiting.")
	} else {
		fmt.Fprintf(w, "%d capability request(s) waiting:\n\n", pending)
	}
	for _, r := range state.Requests {
		if r.Status != "pending" && !all {
			continue
		}
		mark := "pending"
		if r.Status != "pending" {
			mark = r.Status
		}
		fmt.Fprintf(w, "  %s  [%s]  %s", r.ID, mark, r.Capability)
		if r.ToolName != "" {
			fmt.Fprintf(w, " for %s", r.ToolName)
		}
		fmt.Fprintln(w)
		if scope := describeGrantParams(r.Params); scope != "" {
			fmt.Fprintf(w, "        scope: %s\n", scope)
		}
		if r.Reason != "" {
			fmt.Fprintf(w, "        why:   %s\n", r.Reason)
		}
		fmt.Fprintf(w, "        asked: %s by %s\n", r.CreatedAt, shortID(r.AgentID, 8))
	}
	if pending > 0 {
		fmt.Fprintf(w, "\nApprove with `nine grants approve <id>`, or deny with `nine grants deny <id>`.\n")
	}

	fmt.Fprintf(w, "\nCeiling in force (%d grant(s)):\n", len(state.Grants))
	if len(state.Grants) == 0 {
		fmt.Fprintln(w, "  none — a generated tool that declares any capability is refused")
		return
	}
	for _, g := range state.Grants {
		fmt.Fprintf(w, "  %-10s %-9s %s", g.Source, g.Capability, describeGrantParams(g.Params))
		if g.Source == "approved" {
			fmt.Fprintf(w, "  (%s, revoke with `nine grants revoke %s`)", g.ID, g.ID)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w, "\n`config` and `default` grants come from nine.toml and the workspace, and are")
	fmt.Fprintln(w, "rewritten at every boot; `approved` grants are yours and persist until revoked.")
}

// describeGrantParams renders a grant's scope for a human. It reports what the JSON
// says rather than interpreting it, so a shape this build does not know still prints
// something rather than nothing.
func describeGrantParams(params string) string {
	if params == "" {
		return ""
	}
	var p struct {
		Mounts []struct {
			Host  string `json:"host"`
			Guest string `json:"guest"`
		} `json:"mounts"`
		Env        []string `json:"env"`
		AllowHosts []string `json:"allow_hosts"`
		Methods    []string `json:"methods"`
		Scope      string   `json:"scope"`
	}
	if err := json.Unmarshal([]byte(params), &p); err != nil {
		return params
	}
	var parts []string
	for _, m := range p.Mounts {
		parts = append(parts, m.Host+" => "+m.Guest)
	}
	if len(p.Env) > 0 {
		parts = append(parts, strings.Join(p.Env, ","))
	}
	if len(p.AllowHosts) > 0 {
		hosts := strings.Join(p.AllowHosts, ",")
		if len(p.Methods) > 0 {
			hosts = strings.Join(p.Methods, "/") + " " + hosts
		}
		parts = append(parts, hosts)
	}
	if p.Scope != "" {
		parts = append(parts, "scope "+p.Scope)
	}
	if len(parts) == 0 {
		return params
	}
	return strings.Join(parts, "; ")
}
