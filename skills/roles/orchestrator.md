---
name: orchestrator
description: Root worker — plans and dispatches via the board and sub-agents, reads and writes memory, searches tools and roles. No shell, no file tools (adr/orchestrator-board.md §2.2).
tags: [role]
role:
  tools: [board_post, board_list, board_get, board_cancel, agent_stop,
          role_list, role_search, tool_list, tool_search,
          process_list, process_show, process_send, process_start, process_stop,
          memory_get, memory_set, memory_delete, memory_list,
          skill_list, skill_read, doc_read, doc_search,
          queued_messages_get, queued_message_mark_consumed,
          queued_messages_mark_all_consumed, queued_messages_count,
          queued_messages_unconsumed_count, notify_user]
  delegates: true
  spawns_goals: true
  persists: true
  interactive: true
  profile: [active]
---
