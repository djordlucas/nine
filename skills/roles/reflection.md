---
name: reflection
description: Self-reflection session worker — updates the self-model from recent activity; memory tools only.
tags: [role]
role:
  tools: [memory_get, memory_set, memory_delete, memory_list, skill_read]
  delegates: false
  spawns_goals: false
  persists: true
  interactive: false
  profile: [idle-reflection]
---
