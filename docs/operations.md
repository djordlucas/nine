# Backup, restore, and upgrade

Take a snapshot with `nine backup` before every upgrade. A schema migration is
forward-only and applied automatically the first time a newer binary opens the
database, so an upgrade that goes wrong cannot be undone by running the old
binary again — it refuses the newer schema rather than corrupting it.

```sh
nine backup /data/backups/nine-$(date -u +%Y%m%dT%H%M%SZ).tar.gz
```

## Backing up

**The destination's extension names what is captured.**

| Destination | Contains |
|---|---|
| `….db` | The store alone |
| `….tar.gz` | The store **and** the workspace, in one archive |

The daemon can be running either way; it does not need to be stopped and does
not need to be up.

```sh
$ nine backup /data/backups/nine-20260925T171500Z.db
snapshot written: /data/backups/nine-20260925T171500Z.db (4.2 MiB)
source: /data/nine.db

$ nine backup /data/backups/nine-20260925T171500Z.tar.gz
archive written: /data/backups/nine-20260925T171500Z.tar.gz (18.4 MiB)
  store:     /data/nine.db
  workspace: /data/workspace (214 files)
```

Take the archive unless you know the workspace holds nothing you need. The
store references the filesystem — a spill path, a workspace index row — so a
database restored beside a workspace from a different moment describes files
that are not there. Capturing them together is the only way they cannot drift.

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

Restoring an archive brings the workspace back with the store:

```
restored from: /data/backups/nine-20260925T171500Z.tar.gz
  store:     /data/nine.db (schema version 11)
  workspace: /data/workspace (214 files)
  displaced: /data/nine.db.replaced-20260925T181200Z
  displaced: /data/workspace.replaced-20260925T181200Z
undo with: mv /data/nine.db.replaced-20260925T181200Z /data/nine.db
           rm -rf /data/workspace && mv /data/workspace.replaced-20260925T181200Z /data/workspace
```

**Nothing is deleted.** The database being replaced, its sidecars, and the
whole previous workspace directory are renamed with a timestamp — so a restore
aimed at the wrong snapshot is itself reversible, and the command prints the
commands that undo it.

An archive is unpacked and checked in full **before** anything in place is
touched, so a corrupt or foreign archive fails with the live store and
workspace exactly as they were.

The checks run before anything moves, so a refused restore leaves the live
database exactly where it was:

| Refused when | Because |
|---|---|
| The daemon is reachable | It holds the database open; swapping the file underneath corrupts both the restore and the sessions in flight. |
| The source is not a Nine database | An empty or unrelated file opens cleanly in SQLite and would otherwise install as an empty store. |
| An archive holds no `nine.db` | It is not a Nine backup, whatever else is in it. |
| An archive entry escapes the destination | A `../` or absolute path in a tar is how an archive writes outside the directory you named. Nine's own backups never contain one. |
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

## What a backup contains

| | `….db` | `….tar.gz` |
|---|:---:|:---:|
| Conversation history and checkpoints | ✓ | ✓ |
| Goals, workflows, notifications | ✓ | ✓ |
| The event journal | ✓ | ✓ |
| Agent-authored skills and generated tools | ✓ | ✓ |
| The vector store | ✓ | ✓ |
| Workspace files the agent wrote | — | ✓ |
| `.nine/trash/` — deleted and overwritten files | — | ✓ |

The trash is always in the archive. It is what `restore_file` recovers from, so
a backup that dropped it could not undo a deletion — at the cost of carrying
files already marked for removal, bounded by `trash_max_bytes` (1 GiB by
default). Lower that bound if archive size matters more than deep undo.

**Symlinks are skipped**, and the count is reported. A link is a path rather
than content: archiving one would recreate a pointer into the host filesystem
on extract, somewhere it did not exist before.

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
| A `.db` backup is the store alone | Use a `.tar.gz` destination to capture the workspace with it. |
| Archives are whole-tree | Every workspace file is re-archived each time; there is no incremental mode. A large workspace makes a large archive. |
| Symlinks are not preserved | Skipped on archive and absent on restore, deliberately. Their targets are not ours to recreate. |
| Downgrade needs a snapshot | Once a migration runs there is no path back but the file you saved first. |
| No config migration | `nine.toml` has no schema version and no migrate-on-load, so a changed config shape fails at startup rather than being upgraded. |
