# Plugin isolation — environment scrub now, stronger sandboxing deferred

- **Status:** **Environment scrub implemented** (2026-08-04). Everything heavier —
  WebAssembly, an OS-process sandbox, containers — is **deferred by decision**,
  not designed-in. §4 records what was evaluated and why it was set aside, so the
  reasoning survives if the threat model changes.
- **Scope:** plugins run as external OS processes today
  (`internal/plugin`, `exec.Command`). The one host-imposed hardening that costs
  almost nothing, needs no dependency, works on every platform, and requires no
  cooperation from the plugin is: **stop handing the daemon's whole environment
  to spawned plugins.** That is what shipped.

---

## 1. What shipped

Both plugin spawn paths used to do `cmd.Env = append(os.Environ(), …)`, so every
plugin inherited the **daemon's entire environment** — including any secret the
operator exported into the daemon's process (the `database_url` DSN, cloud
credentials, `*_TOKEN` / `*_KEY`). Since Nine loads **user-supplied** plugin
binaries (`internal/plugin/userplugins.go`), that handed arbitrary third-party
code the daemon's secrets for free.

The fix (`internal/plugin/env.go`):

- `sanitizedHostEnv()` returns only an **allowlisted** subset of the daemon's
  environment (`hostEnvAllowlist` + `LC_*` by prefix): `PATH`, `HOME`,
  `TMPDIR`/`TMP`/`TEMP`, `TZ`, `TERM`, `USER`, `LOGNAME`, `LANG`/`LANGUAGE`,
  `LC_*`, and the TLS CA overrides `SSL_CERT_FILE` / `SSL_CERT_DIR`. Enough to
  find executables, resolve temp/locale/timezone, and trust the system CA bundle.
- It replaces `os.Environ()` at both spawn sites: `spawnAndDescribe`
  (`manager.go`, native HTTP-over-socket plugins, and `Probe`) and `newClient`
  (`client.go`, the stdio path used by MCP servers).
- The daemon's **explicit** env is unaffected — `NINE_*`, the cache vars, and
  operator-configured `[plugin.<name>.settings]` still ride in the separate
  `env` / `extraEnv` slice appended after the sanitized base. So a plugin that
  legitimately needs a credential gets it via its settings, not by inheriting the
  daemon's ambient secrets.

## 2. What this is — and is not

It is a **floor**, not a sandbox. It removes the daemon's secrets from the
plugin's environment; it does **not** confine the plugin's filesystem, network,
or syscalls. A malicious plugin can still read whatever files the daemon's user
can read and open its own network connections. The value is proportionate to the
cost: the highest-value secret (the DSN) no longer leaks, for a ~60-line,
zero-dependency, cross-platform change.

If a plugin author needs credentials no longer inherited (e.g. an MCP server that
read an API key from the daemon's env), configure it explicitly via
`[plugin.<name>.settings]` — the intended path.

## 3. Follow-ups if the threat model hardens

Cheap, still host-imposed, additive over the scrub — worth doing only if
"plugins are untrusted" becomes a real requirement:

- Run untrusted plugins under a **restricted uid** + `setrlimit` (memory / CPU /
  fds). Cross-platform-ish, no dependency.
- Linux-only **OS sandbox** (Landlock for filesystem, a network namespace for
  outbound, seccomp for syscalls). Strong, but per-OS and unavailable on macOS —
  see §4.

## 4. Alternatives considered (and deferred)

The design explored several stronger isolation mechanisms before settling on the
scrub. Recorded here so the reasoning is not lost.

- **WebAssembly (wazero).** Run untrusted tools in a capability-sandboxed WASM
  guest — deny-by-default fs/net/env, a portable guarantee identical on macOS and
  Linux. Attractive, but expensive: a bespoke guest-export ABI, linear-memory
  marshalling, and — because a WASM instance has no native I/O or threads — an
  async/streaming host-goroutine model just to reach parity with what a process
  does for free. It also does **not** honor "authors bring plugins in *any*
  language": only languages with a `wasm32-wasi` target, and scripting languages
  only by shipping their interpreter as WASM. Deferred: too much machinery for the
  current need.
- **Extism.** Would remove the worst of the WASM cost — its PDKs provide ready,
  maintained guest SDKs across many languages, and a stable data ABI, so authors
  are not hand-writing wasm plumbing. But it is a large dependency, its
  synchronous call model does not fit the async/streaming tools Nine wants, and
  its built-in network allowlist is not SSRF-hardened to our bar. Deferred with
  WASM.
- **OS-process sandbox (Landlock / namespaces / seccomp).** Keeps the existing
  process model and "any binary, any language" with zero author friction —
  isolation applied at spawn. But the guarantee is **Linux-only**: macOS
  (Seatbelt) is deprecated and awkward, and Nine is a *local* daemon (Unix
  socket) that runs on end-user machines, macOS included — exactly where a
  Linux-only sandbox protects no one. Kept as a possible §3 follow-up, not the
  primary answer.
- **Author-delegated isolation ("wrap your plugin in Docker/WASM yourself").**
  A trap for the security goal: host-vs-plugin isolation **cannot be delegated to
  the party you are defending against** — a malicious author simply declines to
  cage themselves. It is fine and *already possible* as a way for an author to
  isolate their *own* risky sub-work (a plugin may shell to Docker/WASM
  internally; the protocol does not care), but it protects Nine from nothing and
  is not an isolation mechanism.

### The hinge: who is the adversary?

All of the above only pays off if plugin authors are **genuinely untrusted**. If
they are trusted-but-careful (operator-vetted), the env scrub plus normal review
is proportionate and heavier isolation is over-engineering. That question — not
the mechanism — is what should gate any future work here.
