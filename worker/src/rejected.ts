import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { credentialPath } from "./config.js";
import { TerminalLinkError } from "./wire/connection.js";

/** The launcher's argument that marks the installed service (`circus worker install` records it). */
export const SERVICE_FLAG = "--service";

/** `circus internal worker-rejected --credential-only`'s exit status when the credential file is already gone. */
export const CREDENTIAL_GONE = 3;

/** How long `circus internal worker-rejected` may take. */
const REMOVE_TIMEOUT_MS = 60_000;

/** The credential the worker ran with. */
export interface RunningCredential {
  principalId: string;
  token: string;
}

export interface RemovalOutcome {
  /** The helper's exit status: 0 removed, 1 kept, 2 failed, CREDENTIAL_GONE; null without one (`output` says why). */
  status: number | null;
  output: string;
}

/**
 * When the control plane proved it revoked this worker's own identity (a
 * 403 with Circus-Auth: revoked naming `credential`'s principal; never an
 * unknown credential's refusal, a network failure or a refused Hello), asks
 * circus to clean up: `circus internal worker-rejected`, from CIRCUS_BIN or
 * circus on PATH. Run as the installed service (SERVICE_FLAG), circus
 * removes the worker service and the credential file; run in the
 * foreground (`--credential-only`), only the credential file, unless the
 * installed worker service reads it (then it is left to that service), and
 * no service is changed. It passes the token's SHA-256, never the token, and
 * circus keeps the file (and the service) when the file now holds another
 * credential or the installed service reads another file. The worker then
 * exits with its terminal status, which the service manager does not
 * restart. Returns undefined when it does nothing.
 */
export function removeRevoked(
  error: unknown,
  argv: readonly string[],
  credential: RunningCredential | undefined,
  env: NodeJS.ProcessEnv = process.env,
): RemovalOutcome | undefined {
  if (!(error instanceof TerminalLinkError) || error.revokedPrincipal === "") return undefined;
  if (!credential || credential.principalId !== error.revokedPrincipal) {
    return { status: null, output: `the control plane revoked ${error.revokedPrincipal}, not this worker's ${credential?.principalId ?? "credential"}; kept` };
  }
  const tokenHash = createHash("sha256").update(credential.token).digest("hex");
  const scope = argv.includes(SERVICE_FLAG) ? ["--pid", String(process.pid)] : ["--credential-only"];
  const result = spawnSync(env.CIRCUS_BIN || "circus", [
    "internal", "worker-rejected", "--credentials", credentialPath(env), "--principal", credential.principalId,
    "--token-sha256", tokenHash, ...scope,
  ], { env, encoding: "utf8", timeout: REMOVE_TIMEOUT_MS, maxBuffer: 64 << 10, stdio: ["ignore", "pipe", "pipe"] });
  const output = `${result.stdout ?? ""}${result.stderr ?? ""}${result.error ? String(result.error) : ""}`.trim();
  return { status: result.status, output: output.slice(0, 4096) };
}
