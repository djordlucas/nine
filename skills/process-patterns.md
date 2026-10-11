---
name: process-patterns
description: Complete process programs to copy for tool_write — a clock that appends to a file, a database recorder, a summarizer, a change reporter, a reviser — and the mistakes that break them
tags: [patterns, template, example, genesis, heartbeat]
---

## Process patterns

Start from the pattern closest to what you need, change only what differs, and write it
with `tool_write` as shown. Every program here is tested and runs as written.

A process program is one default export that loops forever on `next()`:

- `next()` waits for the next trigger — the `every` clock — and returns `{ kind, at }`;
  `at` is the time, as text.
- `turn(text)` asks the model and returns its reply.
- `report(text)` tells the person (the notification feed).
- Files are under `/work`, the workspace. Declare what the program touches.

### A clock that appends to a file

```js
// pattern: heartbeat — capabilities {"fs":["write"]}
import { next } from "nine:process";
import { appendFile } from "nine:fs";

export default () => {
  for (;;) {
    const t = next();
    appendFile("/work/heartbeat.log", t.at + "\n");
  }
};
```

```json
{"name":"heartbeat","description":"Appends the time to heartbeat.log every 10 seconds.",
 "capabilities":{"fs":["write"]},"source":"<the program>","process":{"every":"10s"}}
```

`appendFile` creates the file the first time. Do not read a file before you have written it.

### Records in the database

```js
// pattern: recorder — capabilities {"sql":true}
import { next } from "nine:process";
import { exec } from "nine:sql";

export default () => {
  exec("CREATE TABLE IF NOT EXISTS readings (at TEXT NOT NULL)");
  for (;;) {
    const t = next();
    exec("INSERT INTO readings (at) VALUES (?)", [t.at]);
  }
};
```

```json
{"name":"recorder","description":"Records a reading every 10 seconds in the readings table.",
 "capabilities":{"sql":true},"source":"<the program>","process":{"every":"10s"}}
```

`exec` writes — `CREATE`, `INSERT`, `UPDATE`, `DELETE`. `query` only reads (`SELECT`) and
returns `{columns, rows}`. Create the table before the loop.

### Ask the model, keep the answer

```js
// pattern: summarizer — capabilities {"fs":["read","write"]}
import { next, turn } from "nine:process";
import { exists, readFileText, writeFile } from "nine:fs";

export default () => {
  for (;;) {
    next();
    if (!exists("/work/notes.md")) continue;
    const notes = readFileText("/work/notes.md");
    const summary = turn("Summarize these notes in three lines:\n\n" + notes);
    writeFile("/work/summary.md", summary);
  }
};
```

```json
{"name":"summarizer","description":"Summarizes notes.md into summary.md every hour.",
 "capabilities":{"fs":["read","write"]},"source":"<the program>","process":{"every":"1h"}}
```

### Report a change to the person

```js
// pattern: reporter — capabilities {"fs":["read"]}
import { next, report } from "nine:process";
import { exists, readFileText } from "nine:fs";

export default () => {
  let last = "";
  for (;;) {
    next();
    if (!exists("/work/status.txt")) continue;
    const now = readFileText("/work/status.txt").trim();
    if (now !== last) {
      report("status changed: " + now);
      last = now;
    }
  }
};
```

```json
{"name":"reporter","description":"Reports when status.txt changes.",
 "capabilities":{"fs":["read"]},"source":"<the program>","process":{"every":"5m"}}
```

A variable like `last` is lost when the process restarts; keep what must last in a file or the
database.

### A reviser

Only when revision is on (`[processes] allow_revision`, and `reviser` in the process roles).

```js
// pattern: reviser — capabilities {}
import { next, turn } from "nine:process";

export default () => {
  for (;;) {
    next();
    turn("Review the processes you wrote: tool_read with no name lists them with their " +
      "state and last error. If one fails or does its job badly, read it, fix that one " +
      "thing, and rewrite it with tool_write under the same name. Otherwise answer NO CHANGE.");
  }
};
```

```json
{"name":"reviser","description":"Reviews and fixes the processes Nine wrote.",
 "source":"<the program>","process":{"every":"6h","role":"reviser"}}
```

### Mistakes that break a process

| Error | Cause | Fix |
|---|---|---|
| `args is not defined` | used a name the program never declared | declare it: `export default (args) => {…}`, or drop it |
| `cannot read /work/x` | read a file that does not exist yet | `exists()` first, or `appendFile` instead of read-then-write |
| `fs.read is not granted` / `fs.write is not granted` | used a file call without declaring it | add `"fs":["read"]` or `"write"` to capabilities |
| `denied: insert into … this is a read` | wrote through `query()` | write with `exec()` |
| `no such table` | queried before creating it | `exec("CREATE TABLE IF NOT EXISTS …")` before the loop |
| `has no export` | imported a name a module lacks | the error lists what it exports |

When a process fails, read its last error with `tool_read`, fix that one line, and write the
whole program again under the same name.
