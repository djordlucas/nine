# Documentation style

Rules for every Markdown file in this repository: `README.md`, `docs/`, `spec/`,
`adr/`, `skills/`, and the `*.d/` READMEs. Read this before writing or editing
any of them.

## The rules

| # | Rule | Test |
|---|------|------|
| 1 | Conclusion first | Can a reader get the answer from the first screen, without reading the rest? |
| 2 | Every sentence carries a fact | Delete the sentence. Did the reader lose information? |
| 3 | Titles name the subject | Does the heading say what the section is about, with no teaser or promise? |
| 4 | Use the right format | Would a table, list, or diagram carry this better than the paragraph? |
| 5 | Limits go last | Does the document end with a `## Limits` section, or state that there are none? |
| 6 | No superseded content | Does anything here describe a past or planned state as if it were current? |
| 7 | Technical terms, defined once | Are the repo's own terms (`docs/glossary.md`) used exactly, and jargon from elsewhere avoided? |

---

## 1. Conclusion first

State the answer, the decision, or the result at the top. Everything below it is
supporting detail that a reader may skip.

This applies at three levels:

- **Document** — the first paragraph after the title states what the document
  concludes. A reader looking up one fact should not have to read to the end.
- **Section** — the first sentence states the point; the rest supports it.
- **Table or list** — the most important column comes first, the most important
  row comes first.

A document that builds an argument toward a reveal is a bad reference document.
Invert it.

```markdown
<!-- Bad: the answer is in the last paragraph -->
# Tool output caps

Tool results vary in size. Some are a few bytes, some are megabytes...
[four paragraphs]
...so results above 32 KiB are written to the store and replaced by a handle.

<!-- Good -->
# Tool output caps

A tool result above 32 KiB is written to the object store and replaced in the
transcript by a handle the model can read back on demand. The cap is
`[tools].max_output_bytes`.
```

## 2. Every sentence carries a fact

Delete any sentence that survives without changing what the reader knows.

Specific patterns to remove:

| Pattern | Example | Fix |
|---------|---------|-----|
| Metaphor as explanation | "This is the load-bearing seam." | Name the actual invariant and what breaks without it. |
| Withheld information | "There are two choices, and not the ones you expect." | State the two choices. |
| Self-commentary | "This is worth stating plainly, because..." | State it. Delete the framing. |
| Significance claims | "This matters because it is the core insight." | If it matters, the fact itself shows that. |
| Restating the heading | Section "Caching" opening with "Nine caches results." | Start with the first real fact. |
| Reader-mind-reading | "You might think X, but actually Y." | State Y. |
| Empty transitions | "With that in mind, let's turn to..." | Delete. Headings are the transitions. |

Rhetorical questions as headings or openers are the same failure — replace them
with the answer.

## 3. Titles

Titles and headings name their subject in the fewest words that stay unambiguous.

| Bad | Good | Why |
|-----|------|-----|
| "How tools reach a turn" | "Tool selection" | Names the mechanism, not a journey. |
| "Sandboxed tools — why it exists, the capability model, and what is deliberately unbuilt" | "Sandboxed tools" | A title is not a summary; put that in the first paragraph. |
| "The two layers, and why neither is enough" | "Ranking and search" | No teaser. |
| "Goal sessions: its cycle, and the bounds on it" | "Goal sessions" | Subtitle after a colon is a headline habit. |

Rules:

- No colon-subtitle teasers, no "what you need to know".
- "Why X" is a valid heading when the section's entire subject is the rationale
  ("Why roles are coarse"). It is not valid as a teaser that withholds the
  answer the section then reveals ("Why this is containable").
- Sentence case, not Title Case.
- A heading is a noun phrase, unless the section documents a procedure — then
  use the imperative ("Add a tool", "Build from source").

## 4. Choosing the format

Pick the vehicle that costs the reader the least.

| Content | Vehicle |
|---------|---------|
| One claim with its reasoning | Paragraph (2–5 sentences) |
| Parallel items, no relationship between them | Bullet list |
| Items with two or more attributes each | Table |
| Config keys, error codes, CLI flags, wire fields | Table with a column per attribute |
| Ordered procedure | Numbered list |
| Process topology, data flow, component wiring | ASCII diagram in a fenced block |
| State machine, lifecycle | Table of transitions, or a diagram if the graph is not linear |
| Exact syntax, payloads, commands | Fenced code block, with the language tagged |

Constraints:

- A bullet list longer than seven items is usually a table with one column
  missing.
- Prose that repeats a table's contents is redundant — delete the prose.
- Every code block declares its language (` ```go `, ` ```toml `, ` ```bash `).
- Diagrams show structure the text cannot; a diagram that restates a list is
  noise.

## 5. Limits

Every document whose subject has open ends ends with a `## Limits` section, as
the last section, covering:

- What is not implemented.
- What is known to be slow, fragile, or incomplete.
- What is scheduled for removal.
- What is deliberately out of scope.

Format as a table when there are several, with the reason:

```markdown
## Limits

| Limit | Detail |
|-------|--------|
| No wasm tool hot-reload | The host loads modules at boot; adding a tool needs a daemon restart. |
| Single SQLite writer | All persistence goes through one file; concurrent writes serialize. |
| `[legacy.retry]` unused | Read by the config parser, ignored by the runtime. Slated for removal. |
```

Rules:

- The section is named `## Limits`, not "Future work", "TODO", "Caveats", or
  "Known issues".
- It goes last. Nothing follows it except cross-reference links, or — in
  `README.md` only — the repository footer: AI use, Contributing, Security,
  License.
- A roadmap is not a limits section. A roadmap lists intended work; `## Limits`
  states what is true today. `README.md` carries both, roadmap first.
- A deliberate non-goal belongs here, marked as deliberate — it stops readers
  filing it as an oversight.
- If a subject genuinely has no open ends, say so in one line rather than
  omitting the section.

## 6. Superseded content

Delete it. Do not annotate it, strike it through, or keep it "for context".

| Situation | Action |
|-----------|--------|
| Behavior changed | Rewrite the statement to the current behavior. |
| Feature removed | Delete its documentation. |
| Plan in `adr/` has shipped | Absorb the behavior into `docs/`; the plan stays in `adr/` as the record. |
| Doc contradicts code | Reconcile it — the mismatch is a bug in one of them, not a doc to follow. |
| Historical decision matters | It belongs in `adr/`, in past tense, not in `docs/`. |

`docs/` and `spec/` describe the present tense only. They carry no roadmap, no
"coming soon", and no future tense except in a `## Limits` section.

## 7. Terminology

- Use the repo's terms exactly as `docs/glossary.md` defines them. Agent,
  session, conversation, sub-agent, goal, workflow and role are distinct; using
  one for another is an error, not a stylistic choice.
- Define a term once, at first use, and link to the glossary rather than
  redefining it.
- Prefer the precise technical term over a plain-language paraphrase:
  "AF_UNIX socket", not "local pipe thing".
- Do not invent metaphors for mechanisms that have names.
- Second person ("you") is allowed in guides and procedures. Reference and
  contract documents use the third person.

## Document types in this repo

| Directory | Tense | Audience | May cite source symbols |
|-----------|-------|----------|-------------------------|
| `docs/` | Present | Users and operators | No — name components and concepts |
| `spec/` | Present, normative | Implementers | Yes, for contracts |
| `adr/` | Past | Maintainers | Yes, freely |
| `skills/` | Imperative | The agent at runtime | No |

`docs/` and `spec/` are embedded in the binary (`docs/embed.go`, `spec/embed.go`),
so they ship to users and must stand alone without the source tree.

## Checklist

Before committing a documentation change:

- [ ] The conclusion is in the first screen.
- [ ] No sentence can be deleted without losing a fact.
- [ ] Headings name subjects; no teasers, no colon-subtitles.
- [ ] Tables carry anything with more than one attribute per item.
- [ ] A `## Limits` section is last, or the subject has no open ends.
- [ ] Nothing describes a superseded state.
- [ ] Glossary terms are used exactly.
- [ ] Code blocks are language-tagged and the commands in them actually run.

## Limits

| Limit | Detail |
|-------|--------|
| Not enforced by CI | No linter checks these rules; review is manual. Rules 3, 5 and 7 are the mechanizable ones if that changes. |
| `adr/` is exempt from rule 6 | ADRs are the historical record. They keep superseded content by design, in past tense, and are not rewritten when the decision they record is later reversed. |
| Style only | This guide does not cover what to document, only how. Coverage rules live in `AGENT.md`. |
