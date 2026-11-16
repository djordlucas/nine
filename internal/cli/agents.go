package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"nine/internal/config"
	"nine/internal/protocol"
)

// Stored agents (adr/agents-concept.md A5): `nine agent …` composes, inspects,
// changes, removes and runs the agent definitions the daemon stores. The web UI
// and POST /api/v1/agents are clients of the same wire verbs.

const agentUsage = "usage: nine agent [list | show <id> | create <name> [--role R] [--skill S]... [--self-model JSON] [--reflection JSON] | edit <id> [--name N] [--role R] [--skill S]... [--self-model JSON] [--reflection JSON] | delete <id> | run <id> | export <id> <path> | import <path> | approve <id>]"

// agentCommand runs `nine agent …`; with no subcommand it lists.
func (c *CLI) agentCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 || args[0] == "list" {
		return c.AgentList(cfg)
	}
	switch args[0] {
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("%s", agentUsage)
		}
		return c.AgentShow(cfg, args[1])
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine agent create <name> [--role R] [--skill S, ...]")
		}
		flags, err := agentFlags(args[2:])
		if err != nil {
			return err
		}
		return c.AgentCreate(cfg, args[1], flags)
	case "edit":
		if len(args) < 2 {
			return fmt.Errorf("%s", agentUsage)
		}
		flags, err := agentFlags(args[2:])
		if err != nil {
			return err
		}
		if len(flags) == 0 {
			return fmt.Errorf("nothing to edit: pass --name/--role/--skill/--self-model/--reflection")
		}
		return c.AgentEdit(cfg, args[1], flags)
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine agent delete <id>")
		}
		return c.AgentDelete(cfg, args[1])
	case "run":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine agent run <id>")
		}
		return c.AgentRun(cfg, args[1])
	case "export":
		if len(args) < 3 {
			return fmt.Errorf("usage: nine agent export <id> <path>")
		}
		return c.AgentExport(cfg, args[1], args[2])
	case "import":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine agent import <path>")
		}
		return c.AgentImport(cfg, args[1])
	case "approve":
		if len(args) < 2 {
			return fmt.Errorf("usage: nine agent approve <id>")
		}
		return c.AgentApprove(cfg, args[1])
	}
	return fmt.Errorf("%s", agentUsage)
}

// agentFlags parses the --key value flags an agent create/edit takes; --skill
// may repeat and collects into "skills" as a JSON array.
func agentFlags(rest []string) (map[string]string, error) {
	flags := map[string]string{}
	var skills []string
	for len(rest) > 0 {
		a := rest[0]
		rest = rest[1:]
		switch a {
		case "--name", "--role", "--self-model", "--reflection":
			if len(rest) == 0 {
				return nil, fmt.Errorf("%s needs a value", a)
			}
			flags[a[2:]] = rest[0]
			rest = rest[1:]
		case "--skill":
			if len(rest) == 0 {
				return nil, fmt.Errorf("--skill needs a value")
			}
			skills = append(skills, rest[0])
			rest = rest[1:]
		default:
			return nil, fmt.Errorf("unknown flag %q", a)
		}
	}
	if len(skills) > 0 {
		raw, _ := json.Marshal(skills)
		flags["skills"] = string(raw)
	}
	return flags, nil
}

// AgentList prints the stored agent definitions.
func (c *CLI) AgentList(cfg *config.Config) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	agents, err := cl.AgentList()
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}
	if len(agents) == 0 {
		fmt.Fprintln(c.Out, "No agents. Compose one with `nine agent create <name>`.")
		return nil
	}
	idW, nameW := len("ID"), len("NAME")
	for _, a := range agents {
		idW, nameW = max(idW, len(a.ID)), max(nameW, len(oneLine(a.Definition.Name)))
	}
	fmt.Fprintf(c.Out, "%-*s %-*s %-12s %s\n", idW, "ID", nameW, "NAME", "ROLE", "VERSION")
	for _, a := range agents {
		fmt.Fprintf(c.Out, "%-*s %-*s %-12s %d\n",
			idW, a.ID, nameW, oneLine(a.Definition.Name), orDash(a.Definition.Role), a.Version)
	}
	return nil
}

// AgentShow prints one agent definition in full, as JSON.
func (c *CLI) AgentShow(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	a, err := cl.AgentGet(id)
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(c.Out, string(raw))
	return nil
}

// AgentCreate composes a stored agent definition.
func (c *CLI) AgentCreate(cfg *config.Config, name string, flags map[string]string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	def := protocol.AgentDefinition{Name: name}
	if v, ok := flags["role"]; ok {
		def.Role = v
	}
	if v, ok := flags["skills"]; ok {
		if err := json.Unmarshal([]byte(v), &def.Skills); err != nil {
			return fmt.Errorf("--skill list is not valid JSON: %w", err)
		}
	}
	if v, ok := flags["self_model"]; ok {
		def.SelfModel = json.RawMessage(v)
	}
	if v, ok := flags["reflection"]; ok {
		def.Reflection = json.RawMessage(v)
	}
	a, err := cl.AgentCreate(def)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}
	fmt.Fprintf(c.Out, "created agent %s (%s) at version %d\n", a.ID, a.Definition.Name, a.Version)
	return nil
}

// AgentEdit patches one stored agent definition.
func (c *CLI) AgentEdit(cfg *config.Config, id string, flags map[string]string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	patch := map[string]any{}
	for k, v := range flags {
		switch k {
		case "name", "role":
			patch[k] = v
		case "skills", "self_model", "reflection":
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(v), &raw); err != nil {
				return fmt.Errorf("--%s is not valid JSON: %w", k, err)
			}
			patch[k] = raw
		}
	}
	a, err := cl.AgentUpdate(id, patch)
	if err != nil {
		return fmt.Errorf("update agent: %w", err)
	}
	fmt.Fprintf(c.Out, "updated agent %s (%s) to version %d\n", a.ID, a.Definition.Name, a.Version)
	return nil
}

// AgentDelete removes one stored agent definition and its versions.
func (c *CLI) AgentDelete(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	if err := cl.AgentDelete(id); err != nil {
		return fmt.Errorf("delete agent: %w", err)
	}
	fmt.Fprintf(c.Out, "deleted agent %s\n", id)
	return nil
}

// AgentExport writes an agent's bundle tar to path.
func (c *CLI) AgentExport(cfg *config.Config, id, path string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	res, err := cl.AgentExport(id)
	if err != nil {
		return fmt.Errorf("export agent: %w", err)
	}
	tarBytes, err := base64.StdEncoding.DecodeString(res.TarBase64)
	if err != nil {
		return fmt.Errorf("decode bundle: %w", err)
	}
	if err := os.WriteFile(path, tarBytes, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(c.Out, "exported agent %s to %s (%d bytes)\n", id, path, len(tarBytes))
	return nil
}

// AgentImport imports a bundle tar from path; the agent lands staged until
// `nine agent approve` (adr/agents-concept.md Q30).
func (c *CLI) AgentImport(cfg *config.Config, path string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	tarBytes, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	a, err := cl.AgentImport(tarBytes)
	if err != nil {
		return fmt.Errorf("import agent: %w", err)
	}
	fmt.Fprintf(c.Out, "imported agent %s (%s) as staged; approve with `nine agent approve %s`\n", a.ID, a.Definition.Name, a.ID)
	return nil
}

// AgentApprove approves a staged (imported) agent.
func (c *CLI) AgentApprove(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	a, err := cl.AgentApprove(id)
	if err != nil {
		return fmt.Errorf("approve agent: %w", err)
	}
	fmt.Fprintf(c.Out, "approved agent %s (%s) — status %s\n", a.ID, a.Definition.Name, a.Status)
	return nil
}

// AgentRun starts a conversation from a stored agent and prints its id.
func (c *CLI) AgentRun(cfg *config.Config, id string) error {
	cl, done, err := c.dialDaemon(cfg)
	if err != nil || cl == nil {
		return err
	}
	defer done()

	res, err := cl.AgentRun(id, true)
	if err != nil {
		return fmt.Errorf("run agent: %w", err)
	}
	role := res.Role
	if role == "" {
		role = "(default)"
	}
	fmt.Fprintf(c.Out, "agent %s is running as conversation %s (role %s)\n", res.AgentID, res.ConversationID, role)
	return nil
}
