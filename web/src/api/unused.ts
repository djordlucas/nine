/**
 * Operations the UI deliberately does not use, each with why (adr/web-ui.md
 * §8). coverage.test.ts fails on an operation neither used nor listed here, and
 * on an entry here that the UI has started to use.
 */
export const unused: Record<string, string> = {
  sendMessage: 'The chat sends a turn over its WebSocket, which also streams the reply and carries answers.',
  streamMessages: 'The SSE form of a turn’s stream; the chat streams over its WebSocket instead.',
  cancelTurn: 'The chat cancels a running turn over its WebSocket.',
  attachSession: 'Attaching registers a terminal client; the chat follows a session over its WebSocket.',
  getGoal: 'A goal shows attached to its pursue process on the Agents page, which reads the whole list.',
  createGoal: 'Goals are created through the API or CLI; in the UI a goal is its process, listed on the Agents page.',
  deleteGoal: 'Same: goals are managed through their process or the API, not a goals page.',
  listWorkflows: 'A workflow shows as a tool-call box in the conversation that runs it; there is no workflow list page.',
  stopWorkflow: 'The workflow’s lifecycle is driven by the agent’s own tools; the UI shows it in the conversation.',
  failWorkflow: 'Same: marked failed by the agent’s tools, shown in the conversation’s workflow box.',
  getSession: 'The Sessions view reads one session from the roster, which carries every field.',
  getHealth: 'The API server’s liveness probe for a load balancer; the UI learns reachability from /auth/session.',
  listAgents: 'Agents are managed from the CLI/API until the Builder UI lands (adr/agents-concept.md A8, phase 6).',
  createAgent: 'Same: composed via `nine agent create` or POST /agents until the Builder UI lands (A8).',
  getAgent: 'Same: agent detail is read via `nine agent show` until the Builder UI lands (A8).',
  updateAgent: 'Same: edited via `nine agent edit` until the Builder UI lands (A8).',
  deleteAgent: 'Same: deleted via `nine agent delete` until the Builder UI lands (A8).',
  runAgent: 'Running an agent opens a conversation the Agents page then shows; no direct UI control until the Builder UI (A8).',
};
