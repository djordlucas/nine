#!/command/with-contenv sh
#
# Runs once as root, before the nine and api services start, and is the only
# thing in the runtime image that needs root.
#
# /data is a volume. Docker creates a named volume root-owned and seeds it from
# the image, and a bind mount arrives with whatever the host gave it — either
# way the image's own `chown nine:nine /data` is shadowed at runtime. Without
# this the daemon starts as uid 1000 and cannot write its database.
#
# Only the volume root and the workspace are touched. A recursive chown across a
# large bind mount would be slow and would rewrite ownership an operator set
# deliberately, so it is done only when the top of /data is not already ours.
set -e

mkdir -p /data/workspace

# /work is the path the sandboxed file tools use for the workspace. Linking it
# here gives `shell` the same name for the same directory, so a path printed by
# one tool resolves in the other. It lands at the filesystem root, which is why
# it is created here rather than in the unprivileged service.
[ -e /work ] || ln -s /data/workspace /work

owner="$(stat -c '%u' /data 2>/dev/null || echo unknown)"
if [ "$owner" != "1000" ]; then
	# A read-only bind mount is a legitimate configuration; say so and carry on
	# rather than failing the boot, since the daemon reports it better.
	chown nine:nine /data /data/workspace 2>/dev/null || \
		echo "init-perms: cannot chown /data (owner uid=$owner); nine runs as uid 1000 and may not be able to write there" >&2
fi
