# Your plugins

Drop your own plugins here. Nine discovers this directory at boot and loads them
alongside the plugins built into the binary. Built-in plugins are unaffected —
this is purely additive.

```
plugins.d/
  weather          the plugin executable
  weather.toml     its manifest (name + entrypoint)
  jira
  jira.toml
```

This directory is scanned at boot; re-scan a running daemon with
`nine plugins reload` (see below). An absent or empty directory just means "no
user plugins". Under Docker it is mounted at `/plugins.d` (see
`docker-compose.yml`).

Enable it for the native layout in `nine.toml`:

```toml
[plugins]
user_dir = "./plugins.d"
```

## What a plugin is

A Nine plugin is a **pre-built executable** that speaks the plugin wire protocol:
on startup it listens on the Unix socket named by `NINE_PLUGIN_SOCKET` and answers
`plugin.describe` (advertising its tools) and `plugin.call`. The easiest way to
write one is with the `nine/internal/plugin` `Serve` helper — see the built-in
plugins under `plugins/` for worked examples (`plugins/time` is the smallest).

Build it however you like and drop the resulting binary here. Nine does **not**
compile anything in this directory.

## The manifest

Every plugin needs a sidecar manifest named `<name>.toml` next to its binary. The
manifest is the gate: **a binary with no manifest beside it is never executed.**

```toml
name = "weather"
entrypoint = "./weather"
```

- `name` — the plugin's identity, shown in `nine plugins`.
- `entrypoint` — path to the executable, resolved relative to the manifest.

The manifest only declares intent — Nine still asks the running plugin for its
real tool list via `plugin.describe`. It does not need to enumerate tools.

## How loading works

At boot (and on `nine plugins reload`), for each manifest, in name order:

1. A malformed manifest or a missing binary is **skipped** — the binary is never
   run.
2. The binary is started and must pass the plugin handshake (`plugin.describe` +
   a matching protocol version). A binary that isn't a plugin is **skipped**.
3. Its tools must not collide with an already-loaded plugin — a built-in **or** an
   earlier user plugin. A collision **skips** the whole plugin; there is no
   overriding a built-in, ever.

Any single failure is logged at ERROR and surfaced in `nine plugins`, but never
aborts the others — one bad drop-in cannot take the daemon down.

## Check before you restart

```sh
nine plugin validate               # checks the configured user_dir
nine plugin validate plugins.d/weather.toml
nine plugin validate ./some-binary
```

`validate` runs the same handshake the daemon uses, locally, without touching a
running daemon — so you can vet a binary before you deploy it.

## Inspect and reload

```sh
nine plugins          # the live roster: built-in + user, with any skip reasons
nine plugins reload   # re-scan this directory without restarting the daemon
```

Reloaded plugins are picked up by new turns; a turn already in flight keeps the
tool set it started with.

(This README is ignored by the loader.)
