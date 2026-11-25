// The Claude Code permission modes a new session can start in
// (`SessionConfig.permission_mode`). An empty mode on the wire is the
// default, `auto`. Plan mode is not offered: a session has no way to
// leave it yet (no `ExitPlanMode`), so it could never change anything.

export const PERMISSION_MODES = ["auto", "default", "acceptEdits", "dontAsk", "bypassPermissions"] as const

export type PermissionMode = (typeof PERMISSION_MODES)[number]

export const DEFAULT_PERMISSION_MODE: PermissionMode = "auto"

export const isPermissionMode = (mode: string): mode is PermissionMode =>
  (PERMISSION_MODES as readonly string[]).includes(mode)

/** A session's mode as the server keeps it: empty is the default. */
export const sessionPermissionMode = (mode: string): string => mode || DEFAULT_PERMISSION_MODE
