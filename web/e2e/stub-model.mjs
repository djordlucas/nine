// A scripted model for the end-to-end tests (adr/web-ui.md §8). It speaks the
// OpenAI-style streaming chat API that nine's mistral provider uses, and
// answers from the last message instead of a model:
//
//   "ask me ..."     -> calls ask_human with two options, then reports the answer
//   "use a tool ..." -> calls a harmless read-only tool, then says it did
//   "slow ..."       -> streams a reply over about 20 s, for cancelling
//   anything else    -> echoes the message, with some Markdown
//
// No model runs in CI; this is what the daemon talks to there.
import http from 'node:http';

const port = Number(process.env.PORT ?? 9000);
// Tools the stub may call for "use a tool": read-only, and present in every
// session.
const harmless = ['skill_list', 'goal_list', 'workflow_list', 'memory_list'];

function text(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) return content.map((c) => c.text ?? '').join('');
  return '';
}

function chunk(delta, finish) {
  return { choices: [{ index: 0, delta, ...(finish ? { finish_reason: finish } : {}) }] };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function reply(req, res, body) {
  const messages = body.messages ?? [];
  const tools = (body.tools ?? []).map((t) => t.function?.name);
  const last = messages[messages.length - 1] ?? {};
  let open = true;
  res.on('close', () => (open = false));

  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
  const send = (o) => open && res.write(`data: ${JSON.stringify(o)}\n\n`);
  const end = (finish) => {
    // Usage rides on the last choice: the provider reads it there.
    send({ ...chunk({}, finish), usage: { prompt_tokens: 100, completion_tokens: 20, total_tokens: 120 } });
    if (open) res.end('data: [DONE]\n\n');
  };
  const say = async (words, gap = 15) => {
    for (const w of words) {
      if (!open) return;
      send(chunk({ content: w }));
      await sleep(gap);
    }
    end('stop');
  };
  const call = (name, args) => {
    send(
      chunk({
        tool_calls: [{ index: 0, id: `call_${Date.now()}`, type: 'function', function: { name, arguments: JSON.stringify(args) } }],
      }),
    );
    end('tool_calls');
  };

  // A tool's result: say what came back.
  if (last.role === 'tool') {
    const asked = messages.findLast((m) => m.role === 'assistant' && m.tool_calls?.length);
    const name = asked?.tool_calls?.[0]?.function?.name;
    if (name === 'ask_human') return say(['You chose ', `**${text(last.content).trim()}**`, '.']);
    return say(['The tool ', `\`${name}\``, ' answered.']);
  }

  const said = text(last.content);
  if (said.includes('ask me') && tools.includes('ask_human')) {
    return call('ask_human', { question: 'Which colour?', options: ['Red', 'Blue'] });
  }
  if (said.includes('use a tool')) {
    const name = harmless.find((t) => tools.includes(t)) ?? tools.find((t) => t && t !== 'ask_human');
    if (name) return call(name, {});
  }
  if (said.includes('slow')) {
    return say(Array.from({ length: 80 }, (_, i) => `tick ${i + 1} `), 250);
  }
  const quoted = said.trim().split('\n').pop().slice(0, 200);
  return say(['Echo: ', `**${quoted}**`, '\n\n', '- one\n', '- two\n\n', 'Some `code` here.']);
}

http
  .createServer((req, res) => {
    if (req.method === 'GET') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      return res.end('{"ok":true}');
    }
    let data = '';
    req.on('data', (d) => (data += d));
    req.on('end', () => {
      let body;
      try {
        body = JSON.parse(data);
      } catch {
        res.writeHead(400);
        return res.end();
      }
      reply(req, res, body).catch((e) => {
        console.error(e);
        if (!res.headersSent) res.writeHead(500);
        res.end();
      });
    });
  })
  .listen(port, () => console.log(`stub model on :${port}`));
