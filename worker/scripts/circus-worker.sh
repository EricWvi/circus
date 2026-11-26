#!/bin/sh
# Starts the circus worker of this package with Node.js 24 or later:
# $CIRCUS_NODE when set, else node on PATH. The worker runs Claude Code
# under `circus internal exec-guard`, from $CIRCUS_BIN when set, else
# circus on PATH.
#
# Without a usable Node.js it exits with the worker's terminal status, so a
# service manager does not restart it: 78 (RestartPreventExitStatus of the
# systemd unit), or 0 on macOS, where launchd restarts every failure.
set -eu

case $(uname -s) in
  Darwin) terminal=0 ;;
  *) terminal=78 ;;
esac

# stop_for_good tells why on stderr and in the worker's stop file, then
# exits with the terminal status. The stop file is the one
# `circus worker install` records in the service (CIRCUS_WORKER_STOP_FILE),
# which `circus worker status` reads; a launcher started by hand has none.
# It is written to a temporary file and renamed, readable by this user
# only; only its own directory is created when missing, never one above it.
stop_for_good() {
  printf 'circus-worker: %s\n' "$1" >&2
  file=${CIRCUS_WORKER_STOP_FILE:-}
  case $file in
    /*)
      message=$(printf '%s' "$1" | tr -d '\000-\037' | sed 's/[\\"]/\\&/g')
      (
        umask 077
        dir=$(dirname "$file")
        [ -d "$dir" ] || mkdir "$dir" || exit 1
        printf '{"reason":"node","message":"%s"}\n' "$message" > "$file.$$" && mv -f "$file.$$" "$file" || rm -f "$file.$$"
      ) 2>/dev/null || true
      ;;
  esac
  exit "$terminal"
}

self=$0
while [ -L "$self" ]; do
  link=$(readlink "$self")
  case $link in
    /*) self=$link ;;
    *) self=$(dirname "$self")/$link ;;
  esac
done
here=$(cd "$(dirname "$self")" && pwd -P)

node=${CIRCUS_NODE:-node}
if ! command -v "$node" >/dev/null 2>&1; then
  stop_for_good "Node.js 24 or later is required: $node not found (install Node.js or set CIRCUS_NODE)"
fi
version=$("$node" -p 'process.versions.node' 2>/dev/null || true)
major=${version%%.*}
case $major in
  '' | *[!0-9]*) major=0 ;;
esac
if [ "$major" -lt 24 ]; then
  stop_for_good "Node.js 24 or later is required: $node is ${version:-unknown} (install Node.js 24 or set CIRCUS_NODE)"
fi

if [ -z "${CIRCUS_BIN:-}" ] && command -v circus >/dev/null 2>&1; then
  CIRCUS_BIN=$(command -v circus)
  export CIRCUS_BIN
fi

exec "$node" "$here/dist/main.js" "$@"
