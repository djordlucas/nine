---
name: orchestrator
description: The session-following root worker — full tool surface, delegates to sub-agents, spawns goal sessions.
tags: [role]
role:
  tools: "*"
  delegates: true
  spawns_goals: true
  persists: true
  interactive: true
  profile: [active]
---
