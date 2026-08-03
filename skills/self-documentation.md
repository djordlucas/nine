---
name: self-documentation
description: How to answer questions about Nine itself by retrieving its own bundled documentation and specification
tags: [docs, spec, self-knowledge, retrieval]
---

## Answering Questions About Nine

Your own manual ships inside your binary: the user documentation (`docs/`) and the
specification (`spec/`) of the exact version you are running. It is not in your
context — retrieve it with `doc_search` and `doc_read`.

**Search before answering any question about how Nine works.** Its architecture,
configuration keys, tools, roles, wire protocol, session plans, goals,
workflows, terminology — all of it is documented, and the documented behavior is
the intended behavior. Answering from your general impression of the system is
how a plausible-sounding wrong answer gets made.

### The two tools

```
doc_search({"query": "how does a turn reach a worker", "top_k": 5})
# → [{"addr": "docs/daemon.md#request-routing", "title": ..., "snippet": ...}, ...]

doc_read({"ref": "docs/daemon.md#request-routing"})   # one section
doc_read({"ref": "agent-loop"})                        # a whole topic
doc_read({"ref": "spec/contracts/wire-protocol"})      # a spec contract
```

`doc_search` returns addresses and snippets so you can pick; `doc_read` returns
the exact text. Read the section, not the whole document, unless the question
really is "explain this topic end to end". Restrict with `"bundle": "spec"` when
you need the precise contract rather than the user-facing explanation.

### Quote, don't paraphrase

When an answer turns on exact behavior — a default value, a field name, an
ordering guarantee, an invariant — quote the documentation and name the address
you took it from (`docs/configuration.md#daemon`). The user can open the same
text with `nine docs configuration`. A paraphrase of a precise contract is how
precision quietly disappears.

### docs vs. spec

| Bundle | Holds | Reach for it when |
|---|---|---|
| `docs` | User-facing explanation: how to use a feature, why it works that way | The question is "how do I…" or "what happens when…" |
| `spec` | Contracts, invariants, wire formats, conformance rules | The question is "what is guaranteed" or you are about to change behavior |

### When the docs and the code disagree

Say so. The documentation and specification are the source of truth for intended
behavior, so a mismatch is a bug to report, not a discrepancy to smooth over.
Quote the documented behavior, state what you observed, and let the human decide
which one is wrong.

### Delegating a broad question

For a question that spans many documents — "how do isolation, plugins, and the
daemon lifecycle fit together?" — consider `run_agent` to read them and report
back, so the intermediate reading stays out of this conversation. Sub-agents
have `doc_search` and `doc_read` too. For a specific lookup, do not delegate:
spawning an agent costs more than the section you were going to read, and the
answer comes back paraphrased when you wanted it quoted.

### What this is not for

These tools serve Nine's own documentation only. For a user's project files use
the file and shell tools; for your own accumulated knowledge use skills and
memory.
