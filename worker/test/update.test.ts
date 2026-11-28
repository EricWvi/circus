import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
import { UpdateMachineSchema } from "@circus/proto/circus/v1/link_pb";
import { WorkerEventSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { HintQueue } from "../src/link/hints.js";
import { WorkerLink } from "../src/link/index.js";
import { SERVICE_FLAG } from "../src/rejected.js";
import {
  cleanText, FEATURE_MACHINE_UPDATE, helloFeatures, MAX_UPDATE_ERROR_BYTES, updateError, Updater,
} from "../src/update.js";
import { ProtocolError } from "../src/wire/codec.js";
import { hello, serve, workerOptions } from "./peer.js";

const log = { debug() {}, info() {}, warn() {}, error() {} };

let dir = "";
let env: NodeJS.ProcessEnv = {};

// A stand-in for circus; each test writes its body.
function fakeCircus(body: string): void {
  writeFileSync(join(dir, "circus"), `#!/bin/sh\n${body}\n`, { mode: 0o755 });
}

beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "circus-update-"));
  env = { CIRCUS_BIN: join(dir, "circus"), PATH: process.env.PATH, HTTPS_PROXY: "http://proxy.test:3128" };
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

function updater(timeoutMs?: number) {
  const reports: { id: string; error: string }[] = [];
  const u = new Updater({
    log, env, timeoutMs,
    report: async (id, error) => { reports.push({ id, error }); },
  });
  return { u, reports };
}

const command = (updateId: string) => create(UpdateMachineSchema, { updateId, version: "v1.2.3" });

const gone = (pid: number): boolean => {
  try {
    process.kill(pid, 0);
    return false;
  } catch {
    return true;
  }
};

describe("Updater", () => {
  it("runs `circus update` with the worker's environment and reports nothing on success", async () => {
    fakeCircus(`printf '%s|%s\\n' "$*" "$HTTPS_PROXY" > "${join(dir, "args")}"\necho updated`);
    const { u, reports } = updater();
    u.update(command("a1"));
    expect(u.running).toBe("a1");
    await u.settled();
    expect(u.running).toBeUndefined();
    expect(readFileSync(join(dir, "args"), "utf8")).toBe("update|http://proxy.test:3128\n");
    expect(reports).toEqual([]);
  });

  it("reports a failure with the cleaned tail of the output, bounded", async () => {
    // Over 2 KiB of output with terminal escapes and control characters.
    fakeCircus(`i=0\nwhile [ $i -lt 200 ]; do printf 'line %03d \\033[31mred\\033[0m\\a\\r\\n' $i; i=$((i+1)); done\necho 'last words' >&2\nexit 3`);
    const { u, reports } = updater();
    u.update(command("a2"));
    await u.settled();
    expect(reports).toHaveLength(1);
    const { id, error } = reports[0];
    expect(id).toBe("a2");
    expect(error.startsWith("circus update exited with status 3\n")).toBe(true);
    expect(error.endsWith("last words")).toBe(true);
    expect(error).toContain("line 199 red");
    expect(error).not.toContain("line 000");
    expect(Buffer.byteLength(error)).toBeLessThanOrEqual(MAX_UPDATE_ERROR_BYTES);
    // eslint-disable-next-line no-control-regex
    expect(error).not.toMatch(/[\x00-\x08\x0b-\x1f\x7f]/);
  });

  it("stops the command and its process group at the deadline and reports it", async () => {
    // The grandchild stands for what circus update may start.
    fakeCircus(`sleep 60 &\necho $! > "${join(dir, "grandchild")}"\necho $$ > "${join(dir, "child")}"\necho started\nwait`);
    const { u, reports } = updater(500);
    u.update(command("a3"));
    await u.settled();
    expect(reports).toHaveLength(1);
    expect(reports[0].error).toBe("circus update was stopped after 1s\nstarted");
    const child = Number(readFileSync(join(dir, "child"), "utf8"));
    const grandchild = Number(readFileSync(join(dir, "grandchild"), "utf8"));
    await expect.poll(() => gone(child) && gone(grandchild), { timeout: 5_000 }).toBe(true);
  });

  it("ignores a command while one runs", async () => {
    fakeCircus(`echo run >> "${join(dir, "runs")}"\nsleep 0.5\nexit 1`);
    const { u, reports } = updater();
    u.update(command("b1"));
    u.update(command("b2"));
    expect(u.running).toBe("b1");
    await u.settled();
    expect(reports.map((r) => r.id)).toEqual(["b1"]);
    expect(readFileSync(join(dir, "runs"), "utf8")).toBe("run\n");
    // Once it ended, a new command runs.
    u.update(command("b3"));
    await u.settled();
    expect(reports.map((r) => r.id)).toEqual(["b1", "b3"]);
  });

  it("takes a new command as soon as the child ended, while its failure is still being reported", async () => {
    fakeCircus(`echo run >> "${join(dir, "runs")}"\nexit 1`);
    const reported: string[] = [];
    let release = () => {};
    const reporting = new Promise<void>((resolve) => { release = resolve; });
    const u = new Updater({
      log, env,
      // The first reply is late: the control plane got the report, the ack is still on its way.
      report: async (id) => { reported.push(id); if (id === "e1") await reporting; },
    });
    u.update(command("e1"));
    await expect.poll(() => reported).toEqual(["e1"]);
    // The retry arrives before that ack: it runs.
    u.update(command("e2"));
    expect(u.running).toBe("e2");
    release();
    await u.settled();
    expect(reported).toEqual(["e1", "e2"]);
    expect(readFileSync(join(dir, "runs"), "utf8")).toBe("run\nrun\n");
  });

  for (const status of [0, 1]) {
    it(`kills what the command left in its process group once it exits (status ${status})`, async () => {
      fakeCircus(`sleep 60 &\necho $! > "${join(dir, "left")}"\nexit ${status}`);
      const { u, reports } = updater();
      u.update(command("f1"));
      await u.settled();
      expect(reports).toHaveLength(status === 0 ? 0 : 1);
      const left = Number(readFileSync(join(dir, "left"), "utf8"));
      await expect.poll(() => gone(left), { timeout: 5_000 }).toBe(true);
    });
  }

  it("reports a command that cannot start", async () => {
    env.CIRCUS_BIN = join(dir, "missing");
    const { u, reports } = updater();
    u.update(command("c1"));
    await u.settled();
    expect(reports).toHaveLength(1);
    expect(reports[0].error).toMatch(/^could not run .*missing update: .*ENOENT/);
  });

  it("drops a report the link cannot send", async () => {
    fakeCircus("exit 1");
    const u = new Updater({ log, env, report: () => Promise.reject(new Error("link down")) });
    u.update(command("d1"));
    await expect(u.settled()).resolves.toBeUndefined();
    expect(u.running).toBeUndefined();
  });
});

describe("update error text", () => {
  it("strips escapes and control characters but keeps newlines and tabs", () => {
    expect(cleanText("a\x1b[1;31mb\x1b[0m\tc\r\nd\x07\x1b]0;title\x07e\x00f\x9b")).toBe("ab\tc\nd" + "ef");
  });

  it("keeps whole characters within the bound", () => {
    const error = updateError("failed", "é".repeat(3000));
    expect(Buffer.byteLength(error)).toBeLessThanOrEqual(MAX_UPDATE_ERROR_BYTES);
    expect(error.startsWith("failed\né")).toBe(true);
    expect(error).not.toContain("�");
    expect(updateError("failed", " \n ")).toBe("failed");
  });
});

describe("machine update on the worker link", () => {
  it("lists the feature only for the installed service", () => {
    expect(helloFeatures([SERVICE_FLAG])).toEqual([FEATURE_MACHINE_UPDATE]);
    expect(helloFeatures([])).toEqual([]);
  });

  it("sends Hello.features and UpdateFailed, and delivers UpdateMachine as a hint", async () => {
    const server = await serve();
    const link = new WorkerLink({ ...workerOptions(server.url), features: [FEATURE_MACHINE_UPDATE] });
    try {
      const hints = link.hints()[Symbol.asyncIterator]();
      const ready = Promise.withResolvers<void>();
      link.onReady(() => ready.resolve());
      link.start();
      const peer = await server.peers.take();
      const h = await hello(peer);
      expect(h.payload).toMatchObject({ case: "hello", value: { features: [FEATURE_MACHINE_UPDATE] } });
      peer.welcome();
      const attach = await peer.receive();
      peer.send({ case: "workerEvent", value: { event: { case: "updateMachine", value: { updateId: "u1", version: "v1.2.3" } } } },
        attach.requestId);
      peer.resume(await peer.receive(), []);
      await ready.promise;
      expect((await hints.next()).value).toMatchObject({ event: { case: "updateMachine", value: { updateId: "u1", version: "v1.2.3" } } });

      const sent = link.updateFailed("u1", "boom");
      const report = await peer.receive();
      expect(report.payload).toMatchObject({ case: "updateFailed", value: { updateId: "u1", error: "boom" } });
      peer.ok(report.requestId);
      await sent;

      const rejected = link.updateFailed("u1", "boom");
      const again = await peer.receive();
      peer.send({ case: "sessionReply", value: { result: { case: "rejected", value: { code: 1, message: "bad" } } } }, again.requestId);
      await expect(rejected).rejects.toThrow("UpdateFailed rejected");
      await hints.return?.();
    } finally {
      await link.close();
      await server.close();
    }
  });

  it("sends no features by default", async () => {
    const server = await serve();
    const link = new WorkerLink(workerOptions(server.url));
    try {
      link.start();
      const h = await hello(await server.peers.take());
      expect(h.payload).toMatchObject({ case: "hello", value: { features: [] } });
    } finally {
      await link.close();
      await server.close();
    }
  });

  it("queues the latest UpdateMachine and refuses an invalid update id", async () => {
    const queue = new HintQueue();
    const update = (updateId: string) => create(WorkerEventSchema, { event: { case: "updateMachine", value: { updateId } } });
    queue.push(update("a"));
    queue.push(update("b"));
    expect(() => queue.push(update(""))).toThrow(ProtocolError);
    expect(() => queue.push(update("bad id"))).toThrow(ProtocolError);
    expect(() => queue.push(update("x".repeat(129)))).toThrow(ProtocolError);
    queue.ready();
    const hints = queue.iterate()[Symbol.asyncIterator]();
    await expect(hints.next()).resolves.toMatchObject({ value: { event: { case: "updateMachine", value: { updateId: "b" } } } });
    const last = hints.next();
    queue.close();
    await expect(last).resolves.toMatchObject({ done: true });
  });
});
