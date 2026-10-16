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
  getGoal: 'The Goals page needs the whole list for the tree, and reads one goal from it.',
  getSession: 'The Sessions view reads one session from the roster, which carries every field.',
  getHealth: 'The API server’s liveness probe for a load balancer; the UI learns reachability from /auth/session.',
};
