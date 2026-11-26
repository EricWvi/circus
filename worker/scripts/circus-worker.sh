#!/bin/sh
# Starts the circus worker of this package with Node.js 24 or later:
# $CIRCUS_NODE when set, else node on PATH. The worker runs Claude Code
# under `circus internal exec-guard`, from $CIRCUS_BIN when set, else
# circus on PATH.
set -eu

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
  echo "circus-worker: Node.js 24 or later is required: $node not found (install Node.js or set CIRCUS_NODE)" >&2
  exit 1
fi
version=$("$node" -p 'process.versions.node' 2>/dev/null || true)
major=${version%%.*}
case $major in
  '' | *[!0-9]*) major=0 ;;
esac
if [ "$major" -lt 24 ]; then
  echo "circus-worker: Node.js 24 or later is required: $node is ${version:-unknown} (install Node.js 24 or set CIRCUS_NODE)" >&2
  exit 1
fi

if [ -z "${CIRCUS_BIN:-}" ] && command -v circus >/dev/null 2>&1; then
  CIRCUS_BIN=$(command -v circus)
  export CIRCUS_BIN
fi

exec "$node" "$here/dist/main.js" "$@"
