import { ConfigError } from "./config.js";
import { TerminalLinkError } from "./wire/connection.js";

/**
 * The exit status for the error that ended the worker. A stop that starting
 * the worker again cannot fix (the control plane refused the credential, or
 * the credential or a setting is missing or invalid) exits 78 on Linux,
 * which the systemd unit `circus worker install` writes does not restart
 * (RestartPreventExitStatus=78), and 0 on macOS, since launchd's KeepAlive
 * can only tell a successful exit from a failed one. The device daemon and
 * the package's launcher use the same statuses. Anything else exits 1 and
 * is restarted.
 */
export function exitStatus(error: unknown, platform: NodeJS.Platform = process.platform): number {
  if (error instanceof TerminalLinkError || error instanceof ConfigError) return platform === "darwin" ? 0 : 78;
  return 1;
}
