import { closeSync, constants, fstatSync, openSync, readSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, resolve } from "node:path";
import { type LogLevel } from "./log.js";

const MAX_CREDENTIAL_BYTES = 64 << 10;

export function readBoundedFile(path: string, limit: number, privateFile = false): Buffer {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NONBLOCK);
  try {
    const info = fstatSync(fd);
    if (!info.isFile() || info.size > limit) throw new Error(`invalid or oversized file: ${path}`);
    if (privateFile && (info.mode & 0o077) !== 0) {
      throw new Error(`credential file must not be accessible by group or others (chmod 600): ${path}`);
    }
    const buffer = Buffer.allocUnsafe(info.size + 1);
    let offset = 0;
    while (offset < buffer.length) {
      const count = readSync(fd, buffer, offset, buffer.length - offset, null);
      if (count === 0) break;
      offset += count;
    }
    if (offset > info.size) throw new Error(`file grew while reading: ${path}`);
    return buffer.subarray(0, offset);
  } finally {
    closeSync(fd);
  }
}

export interface WorkerConfig {
  url: string;
  token: string;
  principalId: string;
  caFile?: string;
  maxSessions: number;
  logLevel: LogLevel;
  /** Idle time before a session's lease is released. */
  idleMs: number;
}

/**
 * A missing or invalid credential (`credential`) or setting (`config`): the
 * worker stops for good (exitStatus), since starting it again cannot fix it.
 */
export class ConfigError extends Error {
  override name = "ConfigError";
  constructor(message: string, readonly kind: "credential" | "config", options?: ErrorOptions) {
    super(message, options);
  }
}

export function loadConfig(env = process.env): WorkerConfig {
  try {
    return readConfig(env);
  } catch (error) {
    if (error instanceof ConfigError) throw error;
    // Everything but the settings comes from reading the credential.
    throw new ConfigError(error instanceof Error ? error.message : String(error), "credential", { cause: error });
  }
}

function readConfig(env: NodeJS.ProcessEnv): WorkerConfig {
  const configHome = env.CIRCUS_HOME
    ? resolve(env.CIRCUS_HOME, "config")
    : join(env.XDG_CONFIG_HOME && isAbsolute(env.XDG_CONFIG_HOME)
      ? env.XDG_CONFIG_HOME
      : join(homedir(), ".config"), "circus");
  const path = join(configHome, "worker.json");
  let data: Buffer;
  try {
    data = readBoundedFile(path, MAX_CREDENTIAL_BYTES, true);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      throw new Error("worker credential missing; run circus join --role worker");
    }
    throw error;
  }
  let credentials: Record<string, unknown>;
  try {
    credentials = JSON.parse(data.toString("utf8"));
  } catch {
    throw new Error("invalid worker credential JSON");
  }
  if (!credentials || credentials.role !== "worker" ||
      typeof credentials.control_plane_url !== "string" || !credentials.control_plane_url ||
      typeof credentials.principal_id !== "string" || !credentials.principal_id ||
      typeof credentials.token !== "string" || !credentials.token ||
      (credentials.ca_file !== undefined && credentials.ca_file !== "" &&
        (typeof credentials.ca_file !== "string" || !isAbsolute(credentials.ca_file)))) {
    throw new Error("invalid worker credential fields");
  }
  const rawMax = env.CIRCUS_WORKER_MAX_SESSIONS ?? "4";
  const maxSessions = Number(rawMax);
  if (!/^\d+$/.test(rawMax) || maxSessions < 1 || maxSessions > 64) {
    throw new ConfigError("CIRCUS_WORKER_MAX_SESSIONS must be an integer from 1 to 64", "config");
  }
  const rawIdle = env.CIRCUS_WORKER_IDLE_SECONDS ?? "300";
  const idleSeconds = Number(rawIdle);
  if (!/^\d+$/.test(rawIdle) || idleSeconds < 1 || idleSeconds > 86_400) {
    throw new ConfigError("CIRCUS_WORKER_IDLE_SECONDS must be an integer from 1 to 86400", "config");
  }
  const logLevel = env.CIRCUS_WORKER_LOG_LEVEL ?? "info";
  if (logLevel !== "debug" && logLevel !== "info" && logLevel !== "warn" && logLevel !== "error") {
    throw new ConfigError("CIRCUS_WORKER_LOG_LEVEL must be debug, info, warn or error", "config");
  }
  return {
    url: credentials.control_plane_url,
    token: credentials.token,
    principalId: credentials.principal_id,
    caFile: typeof credentials.ca_file === "string" ? credentials.ca_file || undefined : undefined,
    maxSessions,
    logLevel,
    idleMs: idleSeconds * 1000,
  };
}
