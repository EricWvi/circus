// The Claude Code permission modes a session can run in
// (`SessionConfig.permission_mode`, the modes the worker accepts). An
// empty mode on the wire is the default, `auto`.

export const PERMISSION_MODES = ["auto", "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"] as const

export type PermissionMode = (typeof PERMISSION_MODES)[number]

export const DEFAULT_PERMISSION_MODE: PermissionMode = "auto"

export const isPermissionMode = (mode: string): mode is PermissionMode =>
  (PERMISSION_MODES as readonly string[]).includes(mode)

/** A session's mode as the server keeps it: empty is the default. */
export const sessionPermissionMode = (mode: string): string => mode || DEFAULT_PERMISSION_MODE
