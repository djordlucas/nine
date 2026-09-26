# Backup, restore, and upgrade

Take a snapshot with `nine backup` before every upgrade. A schema migration is
forward-only and applied automatically the first time a newer binary opens the
database, so an upgrade that goes wrong cannot be undone by running the old
binary again — it refuses the newer schema rather than corrupting it.

```sh
nine backup /data/backups/nine-$(date -u +%Y%m%dT%H%M%SZ).db
```

## Backing up

`nine backup <destination.db>` writes a single, self-contained snapshot. The
daemon can be running; it does not need to be stopped and does not need to be
up.

```sh
$ nine backup /data/backups/nine-20260925T171500Z.db
snapshot written: /data/backups/nine-20260925T171500Z.db (4.2 MiB)
source: /data/nine.db
```

**Do not back up by copying `nine.db`.** The store runs in WAL mode, so that
file alone is not the database — the `-wal` sidecar holds committed
transactions it has not absorbed yet. Copying the three files with `cp` while
the daemon writes reads each at a different instant and produces a torn set;
copying `nine.db` on its own silently loses whatever is still in the WAL.
`nine backup` uses SQLite's `VACUUM INTO`, which reads one transaction and
writes a complete database with no sidecars, so the result is consistent by
construction.

| Property | Behavior |
|---|---|
| Downtime | None. It opens the store read-only and never blocks the daemon's writer. |
| Output | One file. No `-wal` or `-shm` to keep alongside it. |
| Existing destination | Refused. Write each snapshot to a new path. |
| Missing directory | Refused with the directory named. Create it first. |
| Daemon down | Works. The snapshot is taken from the file, not the process. |

The snapshot is a plain SQLite database. Verify one by opening it:

```sh
nine backup /tmp/check.db && ls -la /tmp/check.db
```

## Restoring

Stop the daemon, then `nine restore <snapshot.db>`:

```sh
docker stop nine          # or interrupt a foreground `nine daemon`
nine restore /data/backups/nine-20260925T171500Z.db
docker start nine
```

```
restored: /data/nine.db
from:     /data/backups/nine-20260925T171500Z.db (schema version 11)
displaced: /data/nine.db.replaced-20260925T181200Z
           /data/nine.db-wal.replaced-20260925T181200Z
undo with: mv /data/nine.db.replaced-20260925T181200Z /data/nine.db
```

**Nothing is deleted.** The database being replaced, and its sidecars, are
renamed with a timestamp — so a restore aimed at the wrong snapshot is itself
reversible, and the command prints the `mv` that undoes it.

The checks run before anything moves, so a refused restore leaves the live
database exactly where it was:

| Refused when | Because |
|---|---|
| The daemon is reachable | It holds the database open; swapping the file underneath corrupts both the restore and the sessions in flight. |
| The source is not a Nine database | An empty or unrelated file opens cleanly in SQLite and would otherwise install as an empty store. |
| The snapshot's schema is newer than the binary | Restore it with the version of Nine that wrote it. A schema cannot be migrated backwards. |
| The source *is* the live database | Nothing to do, and the displacement would move the file out from under the copy. |

A snapshot at an **older** schema restores fine; its migrations run when the
daemon next opens it, and the command says so.

There is no stop command for the daemon: `nine daemon` runs in the foreground,
and `nine stop <agent-id>` terminates a *session*, not the process. Stop the
container, or interrupt the foreground process.

### Doing it by hand

If you are recovering somewhere `nine` is not available, the order matters:

```sh
mv /data/nine.db /data/nine.db.displaced
rm -f /data/nine.db-wal /data/nine.db-shm       # stale sidecars of the old file
cp /data/backups/nine-20260925T171500Z.db /data/nine.db
```

**Removing the old `-wal` and `-shm` is the step to not skip.** A snapshot has
no sidecars of its own, and the ones left from the database you replaced belong
to a file that is no longer there. SQLite will pair them with the restored
database and the result is wrong data, with no error. `nine restore` exists
because that failure is silent.

## Upgrading

```sh
nine backup /data/backups/pre-upgrade-$(date -u +%Y%m%dT%H%M%SZ).db
docker pull ghcr.io/djordlucas/nine:latest
# recreate the container against the same /data volume
```

Migrations run automatically when the new binary opens the database, in order,
each one moving the schema forward a single version. Nothing is applied twice
and nothing needs to be invoked.

**Downgrades are refused, not attempted.** An older binary meeting a database a
newer one has migrated stops with:

```
database schema version 12 is newer than this binary understands (11);
upgrade nine rather than downgrading the database
```

That is the safe outcome — it declines rather than writing against a shape it
does not know. It also means the only route back to the previous version is the
snapshot you took first. A release that changes no schema can be rolled back by
image tag alone, but you will not know which kind a release is without checking.

Config is the axis with no migration. `nine.toml` carries no schema version and
is never rewritten by Nine, so a setting that changes shape between versions
breaks at startup rather than being migrated. Read the release notes before
upgrading, and keep the config file under version control.

## What a snapshot contains

Everything in the single SQLite file: conversation history and checkpoints,
goals, workflows, the event journal, agent-authored skills, notifications,
generated tools, and the vector store.

It does **not** contain the workspace filesystem. Files the agent wrote with
`write_file` live on the workspace volume, and `.nine/trash/` with them. Back
that up separately if it holds anything you need.

## Retention: what is already being discarded

Two sweeps bound growth by default, so a long-running instance discards history
whether or not you asked it to. Set them deliberately before a deployment you
intend to keep.

| Setting | Default | Effect |
|---|---|---|
| `[memory] event_retention_turns` | 200 | Keeps the last 200 turns **per agent** in the event journal. A boot-time scrub drops the rest. Negative keeps every turn. |
| `[memory] event_retention_days` | off | Adds an age cap on top of the turn window. |
| `[memory] session_retention_days` | 10 | Deletes an **abandoned** session and everything keyed to it. `nine sessions` marks a protected one "kept". |
| `[memory] trash_retention_days` | 7 | How long a deleted workspace file stays recoverable. Swept hourly, oldest first, also bounded by `trash_max_bytes`. |

The journal default is the one that surprises people. It bounds `nine trace`
and `nine replay`: a turn older than the window is gone from the record, and
the scrub runs at **boot**, so a restart is when history disappears. If the
journal is your audit trail, set `event_retention_turns = -1` and bound it with
`event_retention_days` instead.

## Limits

| Limit | Detail |
|---|---|
| No scheduled backups | `nine backup` is a command, not a timer. Drive it from cron or a systemd timer on the host. |
| Restore needs the daemon stopped | `nine restore` refuses while the socket is reachable. Stopping and starting it is yours to do. |
| Snapshots are not incremental | Each one is a full copy of the database. Size grows with history; prune old snapshots yourself. |
| A long snapshot grows the WAL | `VACUUM INTO` holds a read transaction for its duration, which stops the WAL being checkpointed past that point. Writes continue; the `-wal` sidecar is briefly larger. Only noticeable on a large database. |
| The workspace is not included | Only the SQLite store. Workspace files and the trash need their own backup. |
| Downgrade needs a snapshot | Once a migration runs there is no path back but the file you saved first. |
| No config migration | `nine.toml` has no schema version and no migrate-on-load, so a changed config shape fails at startup rather than being upgraded. |
