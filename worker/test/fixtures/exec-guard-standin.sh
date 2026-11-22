#!/bin/sh
# Stands in for `circus internal exec-guard --parent <pid> -- <command> <args...>`:
# checks the command line, logs it, and runs the command as a child.
[ "$1" = internal ] && [ "$2" = exec-guard ] && [ "$3" = --parent ] && [ "$5" = -- ] || { echo "bad usage: $*" >&2; exit 2; }
[ "$4" = "$PPID" ] || { echo "parent $4 is gone" >&2; exit 125; }
if [ -n "$STANDIN_LOG" ]; then printf '%s\n' "$*" >> "$STANDIN_LOG"; fi
shift 5
if [ -n "$STANDIN_USR1" ]; then
  trap 'echo USR1 >> "$STANDIN_LOG"; echo USR1; [ "$STANDIN_USR1" != exit ] || exit 137' USR1
  echo ready
  for i in $(seq 200); do
    sleep 0.05 & wait "$!"
  done
  exit 1
fi
exec 3<&0
"$@" <&3 &
wait "$!"
