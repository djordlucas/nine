# Worked sandboxed-tool examples

Two complete tools, one per kind. **Nothing here is loaded** — this directory is
not a `[tools].user_dir` and Nine never scans it. Copy what you want into your
own `tools.d/`, which ships empty precisely so that what runs there is what you
chose.

```text
csvstats.toml        the manifest — kind = "js"
csvstats.schema.json { csv: string }
csvstats.js          the code

sha256.toml          the manifest — kind = "wasm"
sha256.schema.json   { text: string }
sha256.c             the source
sha256.wasm          the artifact, 11 KiB, committed
```

## `csv_stats` — the `js` kind

Default-export a function; return a string and it reaches the model untouched,
return anything else and it is JSON-stringified, throw and the model reads your
message as an ordinary tool failure. No build step.

## `sha256` — the `wasm` kind

C compiled to wasm, linking no interpreter. Hashing is the honest demonstration
of why this tier exists: it is exactly the work a language model cannot do by
reasoning about it, and the answer is checkable to the byte.

```console
$ printf 'hello nine' | shasum -a 256
50ce1f9527a47956e94d826d924578d9717c755b14a300ff85a517884d52d035  -
```

It is written against the ABI header, which the binary emits so that it always
matches the ABI that binary implements:

```console
$ nine tool header > nine.h
```

**The `.wasm` is committed, so copying this example needs no C toolchain.** Only
editing `sha256.c` does — `make tools-wasm` then rebuilds it with the wasi-sdk
that `make quickjs-wasm` fetches.

## Using one

```console
$ cp examples/tools/sha256.* tools.d/
$ nine tool validate ./tools.d
  ok    sha256             wasm   declares: none
```

Then turn the subsystem on in `nine.toml` (`[tools] enabled = true`) and restart,
or `nine tools reload` a running daemon.

---

Guide: `nine docs writing-sandboxed-tools`. Design: `nine docs sandboxed-tools`.
Contract: `nine spec toolvm`.
