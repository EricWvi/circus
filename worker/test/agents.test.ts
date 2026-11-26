import { createHash } from "node:crypto";
import { once } from "node:events";
import { existsSync, mkdirSync, mkdtempSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer, type IncomingMessage, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { gzipSync } from "node:zlib";
import { create } from "@bufbuild/protobuf";
import { WorkerEvent_AgentsWantedSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { CLAUDE_CODE_ARTIFACTS, CLAUDE_CODE_VERSION } from "../src/agents/claude-code.generated.js";
import { agentsDirectory, platformKey, registryBase } from "../src/agents/install.js";
import { AgentManager, clipError, claudeCode, type AgentDefinition } from "../src/agents/manager.js";
import { AgentKind, type AgentStatus, AgentStatusSchema, AgentStatus_State } from "@circus/proto/circus/v1/agent_pb";
import { captureLogger, waitFor } from "./runner/harness.js";

// ---- A synthetic npm tarball ----

interface Entry {
  name: string;
  data?: string | Buffer;
  type?: string;
  mode?: number;
  linkname?: string;
}

function header(name: string, size: number, type: string, mode: number, linkname = ""): Buffer {
  const block = Buffer.alloc(512);
  block.write(name, 0, 100, "utf8");
  block.write(mode.toString(8).padStart(7, "0") + "\0", 100, "ascii");
  block.write("0000000\0", 108, "ascii");
  block.write("0000000\0", 116, "ascii");
  block.write(size.toString(8).padStart(11, "0") + "\0", 124, "ascii");
  block.write("00000000000\0", 136, "ascii");
  block.write(type, 156, "ascii");
  block.write(linkname, 157, 100, "utf8");
  block.write("ustar\0", 257, "ascii");
  block.write("00", 263, "ascii");
  block.fill(0x20, 148, 156);
  let sum = 0;
  for (const byte of block) sum += byte;
  block.write(sum.toString(8).padStart(6, "0") + "\0 ", 148, "ascii");
  return block;
}

function tarball(entries: Entry[]): Buffer {
  const parts: Buffer[] = [];
  for (const entry of entries) {
    const data = Buffer.from(entry.data ?? "");
    parts.push(header(entry.name, data.length, entry.type ?? "0", entry.mode ?? 0o644, entry.linkname));
    parts.push(data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  parts.push(Buffer.alloc(1024));
  return gzipSync(Buffer.concat(parts));
}

const integrity = (data: Buffer) => "sha512-" + createHash("sha512").update(data).digest("base64");

const claudeScript = "#!/bin/sh\necho '9.9.9 (Claude Code)'\n";
const goodPackage = () => tarball([
  { name: "package/package.json", data: '{"name":"@anthropic-ai/claude-agent-sdk-test"}' },
  { name: "package/claude", data: claudeScript, mode: 0o755 },
  { name: "package/README.md", data: "readme" },
]);

// ---- A registry stub ----

interface Registry {
  base: string;
  requests: string[];
  /** Holds every response until released (to catch an install in progress). */
  hold?: Promise<void>;
  close(): Promise<void>;
}

async function registry(files: Map<string, Buffer>): Promise<Registry> {
  const reg: Registry = { base: "", requests: [], close: async () => {} };
  const server = createServer((request: IncomingMessage, response: ServerResponse) => {
    reg.requests.push(request.url ?? "");
    const body = files.get(request.url ?? "");
    if (!body) {
      response.writeHead(404).end();
      return;
    }
    response.writeHead(200, { "content-length": body.length, "content-type": "application/octet-stream" });
    // Half now, half when released.
    response.write(body.subarray(0, body.length >> 1));
    void (reg.hold ?? Promise.resolve()).then(() => response.end(body.subarray(body.length >> 1)));
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  reg.base = `http://127.0.0.1:${(server.address() as AddressInfo).port}/registry/`;
  reg.close = () => new Promise((done) => { server.closeAllConnections(); server.close(() => done()); });
  return reg;
}

const PATH = "/registry/@anthropic-ai/claude-agent-sdk-test/-/claude-agent-sdk-test-9.9.9.tgz";

function definition(data: Buffer, version = "9.9.9"): AgentDefinition {
  return {
    kind: AgentKind.CLAUDE_CODE, name: "claude-code", binary: "claude", version,
    artifact: { path: "/@anthropic-ai/claude-agent-sdk-test/-/claude-agent-sdk-test-9.9.9.tgz", integrity: integrity(data) },
  };
}

const wanted = (...agents: Array<{ version: string; attempt?: bigint }>) => create(WorkerEvent_AgentsWantedSchema, {
  agents: agents.map(({ version, attempt }) => ({ agent: AgentKind.CLAUDE_CODE, version, attempt: attempt ?? 1n })),
});

describe("agent installation", () => {
  let root: string;
  let reg: Registry;
  let files: Map<string, Buffer>;
  let reports: AgentStatus[][];
  let busy: boolean;
  const managers: AgentManager[] = [];

  beforeEach(async () => {
    root = mkdtempSync(join(tmpdir(), "circus-agents-"));
    files = new Map();
    reg = await registry(files);
    reports = [];
    busy = false;
  });
  afterEach(async () => {
    await Promise.all(managers.splice(0).map((m) => m.close()));
    await reg.close();
    rmSync(root, { recursive: true, force: true });
  });

  function manager(def: AgentDefinition, progressMs = 60_000): AgentManager {
    const log = captureLogger();
    const m = new AgentManager({
      dir: root, agents: [def], log, busy: () => busy, progressMs,
      env: { CIRCUS_NPM_REGISTRY: reg.base },
      report: async (agents) => { reports.push(agents); },
    });
    managers.push(m);
    return m;
  }
  const dir = () => join(root, "claude-code");
  const states = () => reports.map((r) => r[0].state);
  const status = (m: AgentManager) => m.statuses()[0];

  test("downloads, verifies and renames the version into place, reporting INSTALLING then INSTALLED", async () => {
    const data = goodPackage();
    files.set(PATH, data);
    const m = manager(definition(data), 0);
    await m.start();
    expect(status(m)).toMatchObject({ state: AgentStatus_State.NOT_INSTALLED, targetVersion: "9.9.9", installedVersion: "" });
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBeUndefined();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => status(m).state === AgentStatus_State.INSTALLED, "INSTALLED");
    expect(reg.requests).toEqual([PATH]);
    const binary = join(dir(), "9.9.9", "claude");
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBe(binary);
    expect(statSync(binary).mode & 0o777).toBe(0o755);
    expect(readdirSync(dir())).toEqual(["9.9.9"]);
    expect(states()[0]).toBe(AgentStatus_State.INSTALLING);
    expect(states().at(-1)).toBe(AgentStatus_State.INSTALLED);
    expect(status(m)).toMatchObject({ installedVersion: "9.9.9", error: "", downloadedBytes: 0n });
    // Progress, with the expected total.
    expect(reports.some((r) => r[0].state === AgentStatus_State.INSTALLING && r[0].downloadedBytes > 0n &&
      r[0].totalBytes === BigInt(data.length))).toBe(true);
    // The same wish again changes nothing.
    const before = reports.length;
    m.want(wanted({ version: "9.9.9" }));
    expect(reports.length).toBe(before);
  });

  test("a digest mismatch fails the install and leaves no files", async () => {
    const data = goodPackage();
    files.set(PATH, tarball([{ name: "package/claude", data: "tampered", mode: 0o755 }]));
    const m = manager(definition(data));
    await m.start();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => status(m).state === AgentStatus_State.FAILED, "FAILED");
    expect(status(m).error).toMatch(/^digest mismatch for http:\/\/127\.0\.0\.1:\d+\/registry\/@anthropic-ai\/.*expected sha512-/);
    expect(status(m).installedVersion).toBe("");
    expect(existsSync(dir()) ? readdirSync(dir()) : []).toEqual([]);
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBeUndefined();
    // The user's next install request (a newer attempt) tries again.
    files.set(PATH, data);
    m.want(wanted({ version: "9.9.9", attempt: 2n }));
    await waitFor(() => status(m).state === AgentStatus_State.INSTALLED, "INSTALLED after a retry");
  });

  test("an HTTP error and a package without the binary fail", async () => {
    const noBinary = tarball([{ name: "package/package.json", data: "{}" }]);
    const m = manager(definition(noBinary));
    await m.start();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => status(m).state === AgentStatus_State.FAILED, "FAILED (404)");
    expect(status(m).error).toContain("HTTP 404");
    files.set(PATH, noBinary);
    m.want(wanted({ version: "9.9.9", attempt: 2n }));
    await waitFor(() => status(m).state === AgentStatus_State.FAILED && status(m).error.includes("no claude"), "FAILED (no binary)");
  });

  test.each([
    ["a symbolic link", [{ name: "package/claude", type: "2", linkname: "/bin/sh" }]],
    ["a path leaving the package", [{ name: "package/../claude", data: "x" }]],
    ["an entry outside package/", [{ name: "other/claude", data: "x" }]],
  ])("refuses a tarball with %s", async (_name, entries) => {
    const data = tarball(entries as Entry[]);
    files.set(PATH, data);
    const m = manager(definition(data));
    await m.start();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => status(m).state === AgentStatus_State.FAILED, "FAILED");
    expect(existsSync(join(root, "claude"))).toBe(false);
    expect(existsSync(dir()) ? readdirSync(dir()) : []).toEqual([]);
  });

  test("a leftover temporary directory of an interrupted install is removed at start", async () => {
    const leftover = join(dir(), ".install-abc123");
    mkdirSync(join(leftover, "9.9.9"), { recursive: true });
    writeFileSync(join(leftover, "package.tgz"), "partial");
    const m = manager(definition(goodPackage()));
    await m.start();
    expect(existsSync(leftover)).toBe(false);
    expect(status(m).state).toBe(AgentStatus_State.NOT_INSTALLED);
  });

  test("an install interrupted by a newer wish stops and leaves nothing", async () => {
    const data = goodPackage();
    files.set(PATH, data);
    const release = Promise.withResolvers<void>();
    reg.hold = release.promise;
    const m = manager(definition(data));
    await m.start();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => reg.requests.length === 1, "download started");
    expect(status(m).state).toBe(AgentStatus_State.INSTALLING);
    m.want(wanted());
    await waitFor(() => status(m).state === AgentStatus_State.NOT_INSTALLED, "NOT_INSTALLED");
    release.resolve();
    expect(readdirSync(dir())).toEqual([]);
  });

  test("another version on disk is OUTDATED; installing replaces it", async () => {
    const data = goodPackage();
    files.set(PATH, data);
    mkdirSync(join(dir(), "1.0.0"), { recursive: true });
    writeFileSync(join(dir(), "1.0.0", "claude"), "old");
    const m = manager(definition(data));
    await m.start();
    expect(status(m)).toMatchObject({ state: AgentStatus_State.OUTDATED, installedVersion: "1.0.0", targetVersion: "9.9.9" });
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBeUndefined();
    // Wanted at the version the user asked for before this build: nothing changes.
    m.want(wanted({ version: "1.0.0" }));
    await new Promise((done) => setTimeout(done, 50));
    expect(reg.requests).toEqual([]);
    expect(status(m).state).toBe(AgentStatus_State.OUTDATED);
    // Update.
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => status(m).state === AgentStatus_State.INSTALLED, "INSTALLED");
    expect(readdirSync(dir())).toEqual(["9.9.9"]);
  });

  test("before any AgentsWanted nothing changes on disk", async () => {
    mkdirSync(join(dir(), "9.9.9"), { recursive: true });
    writeFileSync(join(dir(), "9.9.9", "claude"), claudeScript);
    const m = manager(definition(goodPackage()));
    await m.start();
    expect(status(m)).toMatchObject({ state: AgentStatus_State.INSTALLED, installedVersion: "9.9.9" });
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBe(join(dir(), "9.9.9", "claude"));
    expect(reports).toEqual([]);
  });

  test("an uninstall waits until no held session runs the agent, then removes only its directory", async () => {
    mkdirSync(join(dir(), "9.9.9"), { recursive: true });
    writeFileSync(join(dir(), "9.9.9", "claude"), claudeScript);
    const claudeHome = join(root, ".claude");
    mkdirSync(claudeHome);
    writeFileSync(join(claudeHome, "settings.json"), "{}");
    const m = manager(definition(goodPackage()));
    await m.start();
    busy = true;
    m.want(wanted());
    m.sessionsChanged(); // Still busy.
    await new Promise((done) => setTimeout(done, 50));
    expect(status(m).state).toBe(AgentStatus_State.INSTALLED);
    expect(m.executable(AgentKind.CLAUDE_CODE)).toBeDefined();
    busy = false;
    m.sessionsChanged();
    await waitFor(() => status(m).state === AgentStatus_State.NOT_INSTALLED, "NOT_INSTALLED");
    expect(states()).toEqual([AgentStatus_State.UNINSTALLING, AgentStatus_State.NOT_INSTALLED]);
    expect(reports[0][0].installedVersion).toBe("9.9.9");
    expect(existsSync(dir())).toBe(false);
    expect(existsSync(join(claudeHome, "settings.json"))).toBe(true);
  });

  test("an uninstall still waiting is dropped when the agent is wanted again", async () => {
    mkdirSync(join(dir(), "9.9.9"), { recursive: true });
    writeFileSync(join(dir(), "9.9.9", "claude"), claudeScript);
    const m = manager(definition(goodPackage()));
    await m.start();
    busy = true;
    m.want(wanted());
    m.want(wanted({ version: "9.9.9" }));
    busy = false;
    m.sessionsChanged();
    await new Promise((done) => setTimeout(done, 50));
    expect(status(m).state).toBe(AgentStatus_State.INSTALLED);
    expect(reports).toEqual([]);
    expect(existsSync(join(dir(), "9.9.9", "claude"))).toBe(true);
  });

  test("an OUTDATED agent not wanted is uninstalled", async () => {
    mkdirSync(join(dir(), "1.0.0"), { recursive: true });
    writeFileSync(join(dir(), "1.0.0", "claude"), "old");
    const m = manager(definition(goodPackage()));
    await m.start();
    m.want(wanted());
    await waitFor(() => status(m).state === AgentStatus_State.NOT_INSTALLED, "NOT_INSTALLED");
    expect(existsSync(dir())).toBe(false);
  });

  test("a change between building the Attach and readiness is reported once ready", async () => {
    mkdirSync(join(dir(), "9.9.9"), { recursive: true });
    writeFileSync(join(dir(), "9.9.9", "claude"), claudeScript);
    const m = manager(definition(goodPackage()));
    await m.start();
    expect(m.forAttach()[0].state).toBe(AgentStatus_State.INSTALLED);
    m.ready();
    expect(reports).toEqual([]);
    m.forAttach();
    m.want(wanted());
    await waitFor(() => status(m).state === AgentStatus_State.NOT_INSTALLED, "uninstalled");
    const sent = reports.length;
    m.ready();
    await waitFor(() => reports.length === sent + 1, "report at readiness");
    expect(reports.at(-1)![0].state).toBe(AgentStatus_State.NOT_INSTALLED);
    m.forAttach(); // A reconnect carries the latest state: nothing more to report.
    m.ready();
    expect(reports.length).toBe(sent + 1);
  });

  test("a failing report keeps the manager going; the latest state goes next", async () => {
    const data = goodPackage();
    files.set(PATH, data);
    let fail = true;
    const sent: AgentStatus[][] = [];
    const m = new AgentManager({
      dir: root, agents: [definition(data)], log: captureLogger(), busy: () => false, env: { CIRCUS_NPM_REGISTRY: reg.base },
      report: async (agents) => { if (fail) throw new Error("link down"); sent.push(agents); },
    });
    managers.push(m);
    await m.start();
    m.forAttach();
    m.want(wanted({ version: "9.9.9" }));
    await waitFor(() => m.statuses()[0].state === AgentStatus_State.INSTALLED, "INSTALLED");
    fail = false;
    m.ready();
    await waitFor(() => sent.length === 1, "report after readiness");
    expect(sent[0][0].state).toBe(AgentStatus_State.INSTALLED);
  });
});

describe("agent definitions and paths", () => {
  test("Claude Code is pinned to the SDK's CLI with a built-in tarball per platform", () => {
    expect(CLAUDE_CODE_VERSION).toBe("2.1.330");
    expect(Object.keys(CLAUDE_CODE_ARTIFACTS).sort()).toEqual(
      ["darwin-arm64", "darwin-x64", "linux-arm64", "linux-arm64-musl", "linux-x64", "linux-x64-musl"]);
    for (const [platform, artifact] of Object.entries(CLAUDE_CODE_ARTIFACTS)) {
      expect(artifact.package).toBe(`@anthropic-ai/claude-agent-sdk-${platform}`);
      expect(artifact.path).toBe(`/@anthropic-ai/claude-agent-sdk-${platform}/-/claude-agent-sdk-${platform}-0.3.330.tgz`);
      expect(artifact.integrity).toMatch(/^sha512-[A-Za-z0-9+/]{86}==$/);
    }
    expect(claudeCode("linux-x64")).toMatchObject({ name: "claude-code", binary: "claude", version: "2.1.330",
      artifact: { path: CLAUDE_CODE_ARTIFACTS["linux-x64"].path, integrity: CLAUDE_CODE_ARTIFACTS["linux-x64"].integrity } });
    expect(claudeCode("win32-x64").artifact).toBeUndefined();
  });

  test("an agent without an artifact for this platform is not reported", async () => {
    const m = new AgentManager({
      dir: tmpdir(), agents: [claudeCode("win32-x64")], log: captureLogger(), busy: () => false, report: async () => {},
    });
    await m.start();
    expect(m.statuses()).toEqual([]);
  });

  test("platform keys", () => {
    expect(platformKey("linux", "x64", () => false)).toBe("linux-x64");
    expect(platformKey("linux", "arm64", () => true)).toBe("linux-arm64-musl");
    expect(platformKey("darwin", "arm64", () => true)).toBe("darwin-arm64");
    expect(platformKey("win32", "x64", () => false)).toBeUndefined();
    expect(platformKey("linux", "ia32", () => false)).toBeUndefined();
    expect(platformKey()).toBeDefined();
  });

  test("the agents directory follows CIRCUS_HOME, then XDG_DATA_HOME, then ~/.local/share", () => {
    expect(agentsDirectory({ CIRCUS_HOME: "/c", XDG_DATA_HOME: "/x" }, "/home/u")).toBe("/c/agents");
    expect(agentsDirectory({ XDG_DATA_HOME: "/x" }, "/home/u")).toBe("/x/circus/agents");
    expect(agentsDirectory({ XDG_DATA_HOME: "relative" }, "/home/u")).toBe("/home/u/.local/share/circus/agents");
    expect(agentsDirectory({}, "/home/u")).toBe("/home/u/.local/share/circus/agents");
  });

  test("the registry", () => {
    expect(registryBase({})).toBe("https://registry.npmjs.org");
    expect(registryBase({ CIRCUS_NPM_REGISTRY: "http://127.0.0.1:4873/npm/" })).toBe("http://127.0.0.1:4873/npm");
    expect(() => registryBase({ CIRCUS_NPM_REGISTRY: "file:///tmp" })).toThrow("http or https");
  });

  test("errors are cut at a UTF-8 boundary", () => {
    expect(clipError("abc", 2)).toBe("ab");
    expect(clipError("aé", 2)).toBe("a");
    expect(clipError("é".repeat(600))).toBe("é".repeat(512));
  });
});
