# Recorded SDK sequences

Each file is what one real `query()` of `@anthropic-ai/claude-agent-sdk`
0.3.330 (bundled Claude Code 2.1.330) produced against a local fake Messages
API, in arrival order: SDK messages (`msg`), SessionStore appends
(`append`), PreToolUse hook calls (`hook`), the driver's pushes (`push`) and
interrupts (`interrupt`). No real model was called.

The recordings were made with `sessionStoreFlush: 'eager'`,
`includePartialMessages: true` and `settingSources: []`, with permission
mode `bypassPermissions` except `denied` (`auto`, the fake classifier flags
the Bash call). Bulky strings and the `system/init` metadata were trimmed,
paths replaced by `/work` and `/repo` (the uid by `UID`, the git branch by `main`), the session id by `$SESSION` and
pushed uuids by `$PUSH<i>`; SDK retries of a failed append were collapsed
because `test/runner/replay.ts` performs them itself.

| File | Scenario |
| --- | --- |
| `text.json` | One text turn. |
| `tool.json` | One Bash call, then text. |
| `merged.json` | A second input pushed during a Bash call merges into the turn. |
| `now.json` | A `priority: 'now'` input stops a Bash call: empty `result`, then a new turn. |
| `interrupt-stream.json` | `interrupt()` during a stalled text stream, then a follow-up turn. |
| `interrupt-tool.json` | `interrupt()` during a Bash call, then a follow-up turn. |
| `cli-turn.json` | A background Bash task ends while idle and the CLI starts a turn itself. |
| `denied.json` | Auto mode denies a Bash call (`system/permission_denied`, `result.permission_denials`). |
| `mirror.json` | Every attempt of one append fails: `system/mirror_error` before `result`. |
| `held.json` | A slow append holds back `result`. |

Re-record them when the SDK version changes.
