import { createHash } from "node:crypto";
import { createWriteStream, createReadStream, lstatSync, readdirSync } from "node:fs";
import { mkdir, mkdtemp, open, readdir, rename, rm, stat } from "node:fs/promises";
import http from "node:http";
import https from "node:https";
import { homedir } from "node:os";
import { isIP, type LookupFunction, type Socket } from "node:net";
import { isAbsolute, join, resolve } from "node:path";
import tls from "node:tls";
import { Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { createGunzip } from "node:zlib";
import { bareHost, portOf, proxyConfig, proxyFor } from "./proxy.js";
import { extractPackage } from "./tar.js";

// Installing an agent: its npm tarball is downloaded from the registry,
// verified against the digest built into the worker, unpacked into a
// temporary directory next to the install and renamed into
// <agent dir>/<version> in one step. Only the agent's own directory under
// the worker's data directory is ever written or removed.

export const DEFAULT_REGISTRY = "https://registry.npmjs.org";
/** Temporary directories of installs in progress; any left at start are removed. */
const TEMP_PREFIX = ".install-";

/**
 * The worker's agents directory: $CIRCUS_HOME/agents, else
 * $XDG_DATA_HOME/circus/agents (when absolute), else
 * ~/.local/share/circus/agents.
 */
export function agentsDirectory(env: NodeJS.ProcessEnv = process.env, home = homedir()): string {
  if (env.CIRCUS_HOME) return resolve(env.CIRCUS_HOME, "agents");
  const data = env.XDG_DATA_HOME && isAbsolute(env.XDG_DATA_HOME) ? env.XDG_DATA_HOME : join(home, ".local", "share");
  return join(data, "circus", "agents");
}

/** The npm registry tarballs come from: CIRCUS_NPM_REGISTRY, without a trailing slash. */
export function registryBase(env: NodeJS.ProcessEnv = process.env): string {
  const base = (env.CIRCUS_NPM_REGISTRY || DEFAULT_REGISTRY).replace(/\/+$/, "");
  const url = new URL(base);
  if (url.protocol !== "https:" && url.protocol !== "http:") throw new Error("CIRCUS_NPM_REGISTRY must be an http or https URL");
  return base;
}

function isMusl(): boolean {
  const report = process.report?.getReport() as { header?: { glibcVersionRuntime?: string } } | undefined;
  return !report?.header?.glibcVersionRuntime;
}

/** This machine's platform key (process.platform-process.arch, -musl on musl Linux), if agents ship for it. */
export function platformKey(platform = process.platform, arch = process.arch, musl = isMusl): string | undefined {
  if (arch !== "x64" && arch !== "arm64") return undefined;
  if (platform === "darwin") return `darwin-${arch}`;
  if (platform === "linux") return `linux-${arch}${musl() ? "-musl" : ""}`;
  return undefined;
}

/** The versions on disk: subdirectories holding the binary. */
export function installedVersions(dir: string, binary: string): string[] {
  let names: string[];
  try {
    names = readdirSync(dir);
  } catch {
    return [];
  }
  return names.filter((name) => {
    if (name.startsWith(".")) return false;
    try {
      return lstatSync(join(dir, name, binary)).isFile();
    } catch {
      return false;
    }
  }).sort();
}

/** Removes installs interrupted by a stop or crash. */
export async function removeTemporary(dir: string): Promise<void> {
  let names: string[];
  try {
    names = await readdir(dir);
  } catch {
    return;
  }
  for (const name of names) {
    if (name.startsWith(TEMP_PREFIX)) await rm(join(dir, name), { recursive: true, force: true, maxRetries: 2 });
  }
}

/** The bounds of one install. */
export interface InstallLimits {
  /** The largest tarball downloaded; 512 MiB. */
  maxDownloadBytes: number;
  /** The most bytes the package may unpack to; 1 GiB. */
  maxUnpackedBytes: number;
  /** How long the download may go without receiving a byte; 60 s. */
  idleMs: number;
  /** How long the whole install may take; 30 min. */
  deadlineMs: number;
  /** Redirects followed; 5. */
  maxRedirects: number;
}

export const DEFAULT_LIMITS: InstallLimits = {
  maxDownloadBytes: 512 << 20,
  maxUnpackedBytes: 1 << 30,
  idleMs: 60_000,
  deadlineMs: 30 * 60_000,
  maxRedirects: 5,
};

/** n bytes for messages: "512 MiB", "1 GiB", else bytes. */
export function formatBytes(n: number): string {
  if (n >= 1 << 30 && n % (1 << 30) === 0) return `${n / (1 << 30)} GiB`;
  if (n >= 1 << 20 && n % (1 << 20) === 0) return `${n / (1 << 20)} MiB`;
  if (n >= 1 << 10 && n % (1 << 10) === 0) return `${n / (1 << 10)} KiB`;
  return `${n} bytes`;
}

/** ms for messages: "30 min", "60 s", else ms. */
export function formatDuration(ms: number): string {
  if (ms >= 60_000 && ms % 60_000 === 0) return `${ms / 60_000} min`;
  if (ms >= 1_000 && ms % 1_000 === 0) return `${ms / 1_000} s`;
  return `${ms} ms`;
}

export interface InstallRequest {
  /** The agent's directory, e.g. <agents>/claude-code. */
  dir: string;
  version: string;
  /** The binary inside the package, which must be present. */
  binary: string;
  /** The tarball's URL: the registry base and the built-in path. */
  url: string;
  /** "sha512-<base64>", built in. */
  integrity: string;
  signal: AbortSignal;
  /** Called as bytes arrive; total is 0 when unknown. */
  progress?: (downloaded: number, total: number) => void;
  /** Proxy variables (HTTPS_PROXY, NO_PROXY, ...); the process's by default. */
  env?: NodeJS.ProcessEnv;
  /** Bounds; DEFAULT_LIMITS for any left out. */
  limits?: Partial<InstallLimits>;
  /** Extra trusted CA certificates for https downloads (tests). */
  ca?: string | Buffer;
  /** Name resolution for the download's direct connections (tests). */
  lookup?: LookupFunction;
}

/**
 * Fails an install for a cause of its own (idle, deadline): fail() records
 * the first error and aborts signal, so whatever was waiting rejects, and
 * cause() turns any such rejection into that error.
 */
class Watchdog {
  readonly #abort = new AbortController();
  #error?: Error;
  #idle?: ReturnType<typeof setTimeout>;
  readonly #deadline: ReturnType<typeof setTimeout>;
  readonly signal: AbortSignal;

  constructor(outer: AbortSignal, readonly limits: InstallLimits) {
    this.signal = AbortSignal.any([outer, this.#abort.signal]);
    this.#deadline = setTimeout(() => this.fail(new Error(`download took longer than ${formatDuration(limits.deadlineMs)}`)),
      limits.deadlineMs);
    this.alive();
  }

  /** Bytes arrived: the idle timer starts over. */
  alive(): void {
    clearTimeout(this.#idle);
    this.#idle = setTimeout(() => this.fail(new Error(`download timed out (no data for ${formatDuration(this.limits.idleMs)})`)),
      this.limits.idleMs);
  }

  /** The download is done: no more idle timeout (the deadline stays). */
  idleDone(): void {
    clearTimeout(this.#idle);
  }

  fail(error: Error): void {
    this.#error ??= error;
    this.#abort.abort(this.#error);
  }

  /** The error an install failed with: this watchdog's own, else error. */
  cause(error: unknown): unknown {
    return this.#error ?? error;
  }

  stop(): void {
    clearTimeout(this.#idle);
    clearTimeout(this.#deadline);
  }
}

function basicAuth(proxy: URL): Record<string, string> {
  if (!proxy.username && !proxy.password) return {};
  const credentials = `${decodeURIComponent(proxy.username)}:${decodeURIComponent(proxy.password)}`;
  return { "proxy-authorization": "Basic " + Buffer.from(credentials).toString("base64") };
}

/** A CONNECT tunnel to host:port through proxy. */
function tunnel(proxy: URL, host: string, port: string, signal: AbortSignal): Promise<Socket> {
  const authority = host.includes(":") ? `[${host}]:${port}` : `${host}:${port}`;
  return new Promise((done, failed) => {
    const request = http.request({
      host: bareHost(proxy), port: portOf(proxy), method: "CONNECT", path: authority, agent: false, signal,
      headers: { host: authority, ...basicAuth(proxy) },
    });
    request.on("connect", (response, socket, head) => {
      if (response.statusCode !== 200) {
        socket.destroy();
        failed(new Error(`proxy ${proxy.host} refused the tunnel to ${authority}: HTTP ${response.statusCode}`));
        return;
      }
      if (head.length) socket.unshift(head);
      done(socket);
    });
    request.on("error", failed);
    request.end();
  });
}

/** One GET of url, through the environment's proxy for it, without following redirects. */
async function getOnce(url: URL, req: InstallRequest, signal: AbortSignal, watchdog: Watchdog): Promise<http.IncomingMessage> {
  const proxy = proxyFor(url, proxyConfig(req.env ?? process.env));
  const host = bareHost(url);
  let request: http.ClientRequest;
  if (url.protocol === "https:") {
    if (proxy) {
      const socket = await tunnel(proxy, host, portOf(url), signal);
      watchdog.alive();
      // A one-request agent whose connection is TLS over the tunnel.
      const agent = new https.Agent({ maxSockets: 1 });
      agent.createConnection = () => tls.connect({ socket, servername: isIP(host) ? undefined : host, ca: req.ca });
      request = https.get(url, { signal, ca: req.ca, agent });
      request.on("close", () => agent.destroy());
    } else {
      request = https.get(url, { signal, ca: req.ca, agent: false, lookup: req.lookup });
    }
  } else if (url.protocol === "http:") {
    request = proxy
      ? http.get({ host: bareHost(proxy), port: portOf(proxy), path: url.href, agent: false, signal,
        headers: { host: url.host, ...basicAuth(proxy) } })
      : http.get(url, { signal, agent: false, lookup: req.lookup });
  } else {
    throw new Error(`unsupported download URL ${url.protocol}`);
  }
  return new Promise((done, failed) => {
    request.on("response", done);
    request.on("error", failed);
  });
}

/** GET url, following at most limits.maxRedirects redirects (each choosing its proxy again). */
async function get(url: URL, req: InstallRequest, signal: AbortSignal, watchdog: Watchdog): Promise<http.IncomingMessage> {
  for (let redirects = 0; ; redirects++) {
    const response = await getOnce(url, req, signal, watchdog);
    watchdog.alive();
    const status = response.statusCode ?? 0;
    if (status >= 300 && status < 400 && response.headers.location) {
      response.resume();
      if (redirects >= watchdog.limits.maxRedirects) throw new Error(`too many redirects (more than ${watchdog.limits.maxRedirects})`);
      url = new URL(response.headers.location, url);
      continue;
    }
    if (status !== 200) {
      response.resume();
      throw new Error(`download failed: HTTP ${status} from ${url.origin}${url.pathname}`);
    }
    return response;
  }
}

/**
 * Downloads, verifies and unpacks the version into <dir>/<version>,
 * replacing every other version there. On failure nothing of this attempt
 * is left; the caller decides about older versions. The download is
 * bounded in size and time (InstallLimits); each bound fails with an error
 * naming it.
 */
export async function install(req: InstallRequest): Promise<void> {
  const [algorithm, expected] = req.integrity.split("-", 2);
  if (algorithm !== "sha512" || !expected) throw new Error("built-in digest is not sha512");
  const limits = { ...DEFAULT_LIMITS, ...req.limits };
  const tooLarge = () => new Error(`download larger than ${formatBytes(limits.maxDownloadBytes)}`);
  await mkdir(req.dir, { recursive: true, mode: 0o755 });
  const temp = await mkdtemp(join(req.dir, TEMP_PREFIX));
  const watchdog = new Watchdog(req.signal, limits);
  const { signal } = watchdog;
  try {
    const archive = join(temp, "package.tgz");
    const response = await get(new URL(req.url), req, signal, watchdog);
    const total = Number(response.headers["content-length"] ?? 0) || 0;
    if (total > limits.maxDownloadBytes) {
      response.destroy();
      throw tooLarge();
    }
    const hash = createHash("sha512");
    let downloaded = 0;
    const count = new Transform({
      transform(chunk: Buffer, _encoding, callback) {
        watchdog.alive();
        downloaded += chunk.length;
        if (downloaded > limits.maxDownloadBytes) {
          callback(tooLarge());
          return;
        }
        hash.update(chunk);
        req.progress?.(downloaded, total);
        callback(null, chunk);
      },
    });
    await pipeline(response, count, createWriteStream(archive, { flags: "wx", mode: 0o600 }), { signal });
    watchdog.idleDone();
    const actual = hash.digest("base64");
    if (actual !== expected) {
      throw new Error(`digest mismatch for ${req.url}: expected sha512-${expected}, got sha512-${actual}`);
    }
    const unpacked = join(temp, req.version);
    await mkdir(unpacked, { mode: 0o755 });
    const source = createReadStream(archive, { signal });
    const gunzip = createGunzip();
    source.on("error", (error) => gunzip.destroy(error));
    try {
      await extractPackage(source.pipe(gunzip), unpacked, { maxBytes: limits.maxUnpackedBytes });
    } finally {
      source.destroy();
      gunzip.destroy();
    }
    signal.throwIfAborted();
    const binary = join(unpacked, req.binary);
    const info = await stat(binary).catch(() => undefined);
    if (!info?.isFile()) throw new Error(`the package has no ${req.binary}`);
    await rm(archive, { force: true });
    // Only now does anything outside the temporary directory change.
    for (const name of await readdir(req.dir)) {
      if (!name.startsWith(".")) await rm(join(req.dir, name), { recursive: true, force: true, maxRetries: 2 });
    }
    await rename(unpacked, join(req.dir, req.version));
    const handle = await open(req.dir, "r");
    try {
      await handle.sync();
    } finally {
      await handle.close();
    }
  } catch (error) {
    // The caller's own abort (superseded, stopping) stays what it is.
    if (req.signal.aborted) throw req.signal.reason;
    throw watchdog.cause(error);
  } finally {
    watchdog.stop();
    await rm(temp, { recursive: true, force: true, maxRetries: 2 });
  }
}

/** Removes the agent's directory and everything in it. */
export async function uninstall(dir: string): Promise<void> {
  await rm(dir, { recursive: true, force: true, maxRetries: 2 });
}
