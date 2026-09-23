#!/bin/sh
# Container entrypoint, COPYd by the root Dockerfile (doc/tech.md §21.5).
#
# Why this script exists: every path the server writes to is configurable, and a bind mount hands those
# paths over as root-owned, so a process running as `fasttask` cannot create its own database file. This
# derives the directories from the very environment variables the server reads — it never hardcodes a
# second, drifting copy of the defaults — makes them exist with the right owner, and then execs.
#
# It backgrounds nothing. The final `exec` keeps PID 1 semantics, so SIGTERM reaches the server and its
# exit status becomes the container's; that is the property doc/tech.md §21.4 says a shell `&` destroys.
#
# Run the container as root (`docker run --user 0`) only when a bind mount needs its ownership repaired;
# this script drops to `fasttask` before starting the server. The default `USER fasttask` needs no drop.
set -eu

TARGET_USER=fasttask

db_dir=$(dirname "${FASTTASK_DATABASE:-/var/lib/fasttask/fasttask.db}")
audio_dir=${FASTTASK_AUDIO_DIR:-/var/lib/fasttask/audio}
attachment_dir=${FASTTASK_ATTACHMENT_DIR:-/var/lib/fasttask/attachments}
# A spawn-mode sidecar puts its socket next to the database unless the deployment says otherwise
# (internal/bootstrap/sidecar.go), so that directory has to exist and be writable too.
socket_dir=""
if [ "${FASTTASK_SIDECAR_ENABLED:-false}" = "true" ] && [ -n "${FASTTASK_SIDECAR_SOCKET:-}" ]; then
    socket_dir=$(dirname "$FASTTASK_SIDECAR_SOCKET")
fi

# Not `[ ... ] && as_root=1`: under `set -e` a false test at the end of the body would abort the script.
as_root=0
if [ "$(id -u)" -eq 0 ]; then
    as_root=1
fi

# Fail before touching the filesystem. Checking after `chown` would leave half-created directories behind
# and report "invalid user" instead of the actual problem: this is not the image, or the drop target is
# missing, or su-exec is not installed.
if [ "$as_root" -eq 1 ]; then
    if ! command -v su-exec >/dev/null 2>&1; then
        echo "entrypoint: started as root but su-exec is missing; refusing to run the server as root" >&2
        exit 1
    fi
    if ! id -u "$TARGET_USER" >/dev/null 2>&1; then
        echo "entrypoint: user '$TARGET_USER' does not exist here; this script expects the FastTask image" >&2
        exit 1
    fi
fi

# Data files carry one user's goals and attachments: keep them off the group and the world.
umask 0077

printf '%s\n' "$db_dir" "$audio_dir" "$attachment_dir" "$socket_dir" | sort -u |
    while IFS= read -r dir; do
        [ -n "$dir" ] || continue
        [ -d "$dir" ] || mkdir -p "$dir"
        # Recursive: a bind-mounted database directory arrives with root-owned files inside it, and the
        # server has to be able to write the WAL next to them.
        if [ "$as_root" -eq 1 ]; then
            chown -R "$TARGET_USER:$TARGET_USER" "$dir"
        fi
    done

if [ "$as_root" -eq 1 ]; then
    exec su-exec "$TARGET_USER:$TARGET_USER" "$@"
fi

exec "$@"
