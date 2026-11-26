import { spawn, spawnSync } from "node:child_process";
import { once } from "node:events";
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
import { AgentKind, AgentStatus_State, type AgentStatus } from "@circus/proto/circus/v1/agent_pb";
import { WorkerEventSchema, WorkerEvent_AgentsWantedSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, beforeEach, describe, expect, test } from "vitest";
import { AgentsLock, AgentsLockBusy, processStart } from "../src/agents/lock.js";
import { AgentManager, type AgentDefinition, type AgentManagerOptions } from "../src/agents/manager.js";
import { HintQueue } from "../src/link/hints.js";
import { LinkLostError } from "../src/link/index.js";
import { claudeScript, goodPackage, integrity, tarball } from "./agents-fixtures.js";
import { captureLogger, waitFor } from "./runner/harness.js";

const PATH = "/@anthropic-ai/claude-agent-sdk-test/-/claude-agent-sdk-test-9.9.9.tgz";
const definition = (data: Buffer): AgentDefinition => ({
  kind: AgentKind.CLAUDE_CODE, name: "claude-code", binary: "claude", version: "9.9.9",
  artifact: { path: PATH, integrity: integrity(data) },
});
const wanted = (version?: string, attempt = 1n) => create(WorkerEvent_AgentsWantedSchema, {
  agents: version ? [{ agent: AgentKind.CLAUDE_CODE, version, attempt }] : [],
});
/** A pid of a process that has exited (nothing runs with it now). */
function deadPid(): number {
  const child = spawnSync(process.execPath, ["-e", "process.stdout.write(String(process.pid))"], { encoding: "utf8" });
  return Number(child.stdout);
}
/** A pid of a live process other than this one. */
const otherLivePid = () => process.ppid;

let root: string;
let server: Server;
let base: string;
let body: Buffer | undefined;
let gets = 0;
const managers: AgentManager[] = [];

beforeEach(async () => {
  root = mkdtempSync(join(tmpdir(), "circus-manager-"));
  body = undefined;
  gets = 0;
  server = createServer((_req, res) => {
    gets++;
    if (!body) res.writeHead(404).end();
    else res.writeHead(200, { "content-length": body.length }).end(body);
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  base = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});
afterEach(async () => {
  await Promise.all(managers.splice(0).map((m) => m.close()));
  server.closeAllConnections();
  await new Promise((done) => server.close(done));
  chmodRecursive(root);
  rmSync(root, { recursive: true, force: true });
});

function chmodRecursive(dir: string): void {
  if (!existsSync(dir)) return;
  chmodSync(dir, 0o755);
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory()) chmodRecursive(join(dir, entry.name));
  }
}

function manager(data: Buffer, opts: Partial<AgentManagerOptions> = {}): AgentManager {
  const m = new AgentManager({
    dir: root, agents: [definition(data)], log: captureLogger(), busy: () => false,
    env: { CIRCUS_NPM_REGISTRY: base }, report: async () => {}, retryMs: { first: 20, max: 40, maxUninstall: 40 }, ...opts,
  });
  managers.push(m);
  return m;
}
const agentDir = () => join(root, "claude-code");
const state = (m: AgentManager) => m.statuses()[0].state;
const installAt = (version: string) => {
  mkdirSync(join(agentDir(), version), { recursive: true });
  writeFileSync(join(agentDir(), version, "claude"), claudeScript);
};

describe("the agents directory lock", () => {
  test("a second holder waits for the first, and gives up naming it", async () => {
    const first = new AgentsLock(root, otherLivePid());
    const second = new AgentsLock(root);
    const held = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const holding = first.run(async () => {
      held.resolve();
      await release.promise;
    }, 1_000);
    await held.promise;
    await expect(second.run(async () => "never", 300)).rejects.toThrow(AgentsLockBusy);
    await expect(second.run(async () => "never", 300)).rejects.toThrow(`locked by process ${otherLivePid()} on ${hostname()}`);
    const waiting = second.run(async () => "after", 5_000);
    release.resolve();
    await holding;
    await expect(waiting).resolves.toBe("after");
    expect(existsSync(join(root, ".lock"))).toBe(false);
  });

  test("a lock left by a process that is gone is taken; one of another host is not", async () => {
    writeFileSync(join(root, ".lock"), JSON.stringify({ pid: deadPid(), host: hostname() }));
    await expect(new AgentsLock(root).run(async () => "taken", 0)).resolves.toBe("taken");
    writeFileSync(join(root, ".lock"), JSON.stringify({ pid: deadPid(), host: "elsewhere" }));
    await expect(new AgentsLock(root).run(async () => "taken", 200)).rejects.toThrow("on elsewhere");
  });

  describe("stale locks", () => {
    const lockFile = () => join(root, ".lock");
    const backdate = (ms: number) => {
      const then = new Date(Date.now() - ms);
      utimesSync(lockFile(), then, then);
    };
    const timing = { refreshMs: 50, staleMs: 1_000 };
    const cases: Array<[string, () => string]> = [
      ["a dead pid on another host", () => JSON.stringify({ pid: deadPid(), host: "old-container-id" })],
      ["an empty file", () => ""],
      ["a live unrelated pid on this host", () => JSON.stringify({ pid: otherLivePid(), host: hostname() })],
      ["a damaged file", () => "{\"pid\": 1"],
    ];
    test.each(cases)("%s: held while fresh, taken over once older than the stale time", async (_name, content) => {
      mkdirSync(root, { recursive: true });
      writeFileSync(lockFile(), content());
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 300)).rejects.toThrow(AgentsLockBusy);
      backdate(2_000);
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 300)).resolves.toBe("taken");
      expect(existsSync(lockFile())).toBe(false);
      expect(existsSync(join(root, ".lock.takeover"))).toBe(false);
    });

    test("a fresh lock naming this pid and host that this process does not hold is taken over at once", async () => {
      writeFileSync(lockFile(), JSON.stringify({ pid: process.pid, host: hostname() }));
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 0)).resolves.toBe("taken");
    });

    test("a holder refreshes the lock, so it never looks stale while held", async () => {
      const holder = new AgentsLock(root, otherLivePid(), undefined, timing);
      const held = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      const holding = holder.run(async () => {
        held.resolve();
        await release.promise;
      }, 1_000);
      await held.promise;
      backdate(5_000);
      // Within a refresh the mtime is current again.
      await new Promise((done) => setTimeout(done, 150));
      expect(Date.now() - statSync(lockFile()).mtimeMs).toBeLessThan(1_000);
      // Another holder (an instance standing for another process) waits past the stale time and is still refused.
      await expect(new AgentsLock(root, deadPid(), "elsewhere", timing).run(async () => "taken", 1_500)).rejects.toThrow(AgentsLockBusy);
      release.resolve();
      await holding;
      expect(existsSync(lockFile())).toBe(false);
    });

    test("a holder of this process is waited for, not taken over, though its file names this pid", async () => {
      const first = new AgentsLock(root, undefined, undefined, timing);
      const held = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      const holding = first.run(async () => {
        held.resolve();
        await release.promise;
      }, 1_000);
      await held.promise;
      const content = readFileSync(lockFile(), "utf8");
      const second = new AgentsLock(root, undefined, undefined, timing);
      await expect(second.run(async () => "never", 300)).rejects.toThrow(`locked by process ${process.pid} on ${hostname()}`);
      expect(readFileSync(lockFile(), "utf8")).toBe(content);
      // Even with the file gone meanwhile, it waits for the holder (no spin, no second holder).
      rmSync(lockFile());
      const order: string[] = [];
      const waiting = second.run(async () => { order.push("second"); }, 5_000);
      await new Promise((done) => setTimeout(done, 100));
      order.push("first released");
      release.resolve();
      await holding;
      await waiting;
      expect(order).toEqual(["first released", "second"]);
    });

    test("a takeover removes only the stale file it inspected, not one created meanwhile", async () => {
      writeFileSync(lockFile(), "");
      backdate(2_000);
      const fresh = JSON.stringify({ pid: otherLivePid(), host: "another-host" });
      const taker = new AgentsLock(root, undefined, undefined, {
        ...timing,
        // Between the inspection and the takeover, the stale holder's file
        // was replaced by another process's fresh lock.
        beforeTakeover: async () => {
          rmSync(lockFile());
          writeFileSync(lockFile(), fresh);
        },
      });
      await expect(taker.run(async () => "taken", 300)).rejects.toThrow("on another-host");
      expect(readFileSync(lockFile(), "utf8")).toBe(fresh);
      expect(existsSync(join(root, ".lock.takeover"))).toBe(false);
    });

    test("a holder releasing after its lock was taken over leaves the new holder's file", async () => {
      const holder = new AgentsLock(root, undefined, undefined, timing);
      const other = JSON.stringify({ pid: otherLivePid(), host: "another-host" });
      await holder.run(async () => {
        // It stalled past the stale time; another process took the lock over.
        rmSync(lockFile());
        writeFileSync(lockFile(), other);
      }, 1_000);
      expect(readFileSync(lockFile(), "utf8")).toBe(other);
    });

    test("a holder taken over stops refreshing, so the new holder's lock goes stale once that one stops", async () => {
      const holder = new AgentsLock(root, undefined, undefined, timing);
      const other = JSON.stringify({ pid: deadPid(), host: "another-host" });
      await holder.run(async () => {
        // Taken over by a holder on another host, which then died: its lock
        // is no longer refreshed.
        rmSync(lockFile());
        writeFileSync(lockFile(), other);
        backdate(5_000);
        // Several refresh periods of the first holder pass.
        await new Promise((done) => setTimeout(done, 300));
        expect(Date.now() - statSync(lockFile()).mtimeMs).toBeGreaterThan(4_000);
      }, 1_000);
      // The first holder left the file alone, and it is stale: taken over.
      expect(readFileSync(lockFile(), "utf8")).toBe(other);
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 0)).resolves.toBe("taken");
      expect(existsSync(lockFile())).toBe(false);
    });

    test("a live process of this host with the recorded start time is never taken over, however old the lock", async () => {
      const start = processStart(otherLivePid());
      expect(start).toBeTruthy();
      writeFileSync(lockFile(), JSON.stringify({ pid: otherLivePid(), host: hostname(), start }));
      backdate(3_600_000);
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 300))
        .rejects.toThrow(`locked by process ${otherLivePid()} on ${hostname()}`);
      // The pid runs, but another process started with it: reused, stale at once.
      writeFileSync(lockFile(), JSON.stringify({ pid: otherLivePid(), host: hostname(), start: `${start}-earlier` }));
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 0)).resolves.toBe("taken");
      // A dead pid with a start time: stale at once too.
      writeFileSync(lockFile(), JSON.stringify({ pid: deadPid(), host: hostname(), start }));
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 0)).resolves.toBe("taken");
    });

    test.skipIf(process.platform !== "linux")("a holder paused past the stale time (SIGSTOP) keeps the lock on its host", async () => {
      const marker = join(root, "held");
      const script = `
        const { AgentsLock } = await import(${JSON.stringify(new URL("../src/agents/lock.ts", import.meta.url).href)});
        const { writeFileSync } = await import("node:fs");
        await new AgentsLock(${JSON.stringify(root)}, undefined, undefined, { refreshMs: 100, staleMs: 500 }).run(async () => {
          writeFileSync(${JSON.stringify(marker)}, "");
          // A pending promise alone does not keep Node running (the lock's
          // refresh timer is unref'd): an interval does, until it is killed.
          await new Promise(() => setInterval(() => {}, 60_000));
        }, 10000);`;
      const child = spawn(process.execPath, ["--experimental-strip-types", "--input-type=module", "-e", script], { stdio: "inherit" });
      const exited = once(child, "exit");
      try {
        await waitFor(() => existsSync(marker), "child holds the lock");
        child.kill("SIGSTOP");
        await new Promise((done) => setTimeout(done, 1_500));
        expect(Date.now() - statSync(lockFile()).mtimeMs).toBeGreaterThan(500);
        // Still the holder, paused: not exited after writing the marker.
        expect(readFileSync(`/proc/${child.pid}/stat`, "utf8")).toMatch(/\) T /);
        await expect(new AgentsLock(root, undefined, undefined, { refreshMs: 100, staleMs: 500 }).run(async () => "taken", 500))
          .rejects.toThrow(`locked by process ${child.pid} on ${hostname()}`);
      } finally {
        child.kill("SIGKILL");
        await exited;
      }
      // Gone: its lock is taken at once.
      await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 0)).resolves.toBe("taken");
    });

    describe("takeover guards", () => {
      const guard = () => join(root, ".lock.takeover");
      const age = (path: string, ms: number) => {
        const then = new Date(Date.now() - ms);
        utimesSync(path, then, then);
      };

      test("a fresh guard left behind does not keep an acquirer past its own wait", async () => {
        writeFileSync(lockFile(), "");
        backdate(120_000);
        writeFileSync(guard(), "");
        const started = performance.now();
        await expect(new AgentsLock(root).run(async () => "taken", 1_500)).rejects.toThrow(AgentsLockBusy);
        expect(performance.now() - started).toBeLessThan(5_000);
        expect(existsSync(guard())).toBe(true);
      });

      test("a guard left by a crashed taker is cleared once stale, and the stale lock taken over", async () => {
        writeFileSync(lockFile(), "");
        backdate(2_000);
        writeFileSync(guard(), "");
        age(guard(), 2_000);
        await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => "taken", 1_000)).resolves.toBe("taken");
        expect(existsSync(guard())).toBe(false);
        expect(existsSync(lockFile())).toBe(false);
      });

      test("a fresh guard that replaced the stale one meanwhile is put back, not removed", async () => {
        writeFileSync(lockFile(), "");
        backdate(2_000);
        writeFileSync(guard(), "old");
        age(guard(), 2_000);
        let replaced = false;
        const taker = new AgentsLock(root, undefined, undefined, {
          ...timing,
          beforeGuardClear: async () => {
            if (replaced) return;
            replaced = true;
            rmSync(guard());
            writeFileSync(guard(), "another taker's");
          },
        });
        await expect(taker.run(async () => "taken", 300)).rejects.toThrow(AgentsLockBusy);
        expect(readFileSync(guard(), "utf8")).toBe("another taker's");
      });

      test("while one taker holds the guard, another cannot take the stale lock over", async () => {
        writeFileSync(lockFile(), "");
        backdate(2_000);
        const inTakeover = Promise.withResolvers<void>();
        const proceed = Promise.withResolvers<void>();
        const order: string[] = [];
        const first = new AgentsLock(root, undefined, undefined, {
          ...timing,
          beforeTakeover: async () => {
            inTakeover.resolve();
            await proceed.promise;
          },
        });
        const firstRun = first.run(async () => { order.push("first"); }, 5_000);
        await inTakeover.promise;
        await expect(new AgentsLock(root, undefined, undefined, timing).run(async () => { order.push("second"); }, 300))
          .rejects.toThrow(AgentsLockBusy);
        proceed.resolve();
        await firstRun;
        expect(order).toEqual(["first"]);
      });
    });

    test("two processes taking over one stale lock: one holds it at a time", async () => {
      writeFileSync(lockFile(), "");
      backdate(2_000);
      const script = (tag: string) => `
        const { AgentsLock } = await import(${JSON.stringify(new URL("../src/agents/lock.ts", import.meta.url).href)});
        const { appendFileSync } = await import("node:fs");
        await new AgentsLock(${JSON.stringify(root)}, undefined, undefined, { refreshMs: 50, staleMs: 1000 }).run(async () => {
          appendFileSync(${JSON.stringify(join(root, "log"))}, "${tag}+\\n");
          await new Promise((done) => setTimeout(done, 200));
          appendFileSync(${JSON.stringify(join(root, "log"))}, "${tag}-\\n");
        }, 10000);`;
      const run = (tag: string) => new Promise<number>((done) => {
        const child = spawn(process.execPath, ["--experimental-strip-types", "--input-type=module", "-e", script(tag)], { stdio: "inherit" });
        child.on("exit", (code) => done(code ?? 1));
      });
      expect(await Promise.all([run("a"), run("b")])).toEqual([0, 0]);
      const log = readFileSync(join(root, "log"), "utf8").trim().split("\n");
      expect(log).toHaveLength(4);
      // Strictly alternating: a holder's end comes before the other's start.
      expect(log[0][0]).toBe(log[1][0]);
      expect(log[2][0]).toBe(log[3][0]);
      expect(log[1].endsWith("-") && log[3].endsWith("-")).toBe(true);
    });
  });

  test("holders of one process take turns", async () => {
    const lock = new AgentsLock(root);
    const order: string[] = [];
    await Promise.all(["a", "b", "c"].map((name) => lock.run(async () => {
      order.push(name + "+");
      await new Promise((done) => setTimeout(done, 10));
      order.push(name + "-");
    }, 1_000)));
    expect(order).toEqual(["a+", "a-", "b+", "b-", "c+", "c-"]);
  });

  test("while another process holds it, a starting worker leaves temporary directories alone and an install waits, then fails clearly", async () => {
    const other = new AgentsLock(root, otherLivePid());
    const held = Promise.withResolvers<void>();
    const release = Promise.withResolvers<void>();
    const holding = other.run(async () => {
      held.resolve();
      await release.promise;
    }, 1_000);
    await held.promise;
    installAt("9.9.9");
    const theirs = join(agentDir(), ".install-theirs");
    mkdirSync(theirs);
    const data = goodPackage();
    body = data;
    const m = manager(data, { lockWaitMs: 200 });
    await m.start();
    expect(existsSync(theirs)).toBe(true);
    // An install (an Update of a damaged install) cannot take the lock.
    rmSync(join(agentDir(), "9.9.9", "claude"));
    installAt("1.0.0");
    await m.start();
    m.want(wanted("9.9.9"));
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED");
    expect(m.statuses()[0].error).toMatch(/locked by process \d+/);
    expect(gets).toBe(0);
    expect(readdirSync(agentDir()).sort()).toEqual([".install-theirs", "1.0.0", "9.9.9"]);
    release.resolve();
    await holding;
  });
});

describe("two processes on one data directory", () => {
  test("an install finds the target another process installed under the lock, and neither downloads nor removes it", async () => {
    const data = goodPackage();
    body = Buffer.from("not the package");
    const late = manager(data);
    await late.start();
    expect(state(late)).toBe(AgentStatus_State.NOT_INSTALLED);
    // The other process installs while this one still has the state it read.
    installAt("9.9.9");
    late.want(wanted("9.9.9"));
    await waitFor(() => state(late) === AgentStatus_State.INSTALLED, "INSTALLED");
    expect(gets).toBe(0);
    expect(existsSync(join(agentDir(), "9.9.9", "claude"))).toBe(true);
  });
});

describe("reports", () => {
  test("a report that fails while the link is up is sent again with backoff, with the latest state", async () => {
    installAt("9.9.9");
    const sent: AgentStatus[][] = [];
    let failures = 2;
    const m = manager(goodPackage(), {
      report: async (agents) => {
        if (failures-- > 0) throw new Error("ReportAgents rejected (code STORE_UNAVAILABLE): store unavailable; retry");
        sent.push(agents);
      },
    });
    await m.start();
    m.want(wanted());
    await waitFor(() => sent.length >= 1, "a report after two failures");
    expect(sent.at(-1)![0].state).toBe(AgentStatus_State.NOT_INSTALLED);
    const count = sent.length;
    await new Promise((done) => setTimeout(done, 150));
    expect(sent.length).toBe(count); // No more retries once it went through.
  });

  test("a report that fails because the link is down waits for readiness", async () => {
    installAt("9.9.9");
    let calls = 0;
    let down = true;
    const m = manager(goodPackage(), {
      report: async () => {
        calls++;
        if (down) throw new LinkLostError("worker link is disconnected");
      },
    });
    await m.start();
    m.forAttach();
    m.want(wanted());
    await waitFor(() => state(m) === AgentStatus_State.NOT_INSTALLED, "uninstalled");
    const before = calls;
    await new Promise((done) => setTimeout(done, 150));
    expect(calls).toBe(before);
    down = false;
    m.ready();
    await waitFor(() => calls === before + 1, "a report at readiness");
  });

  test("a change while a report is in flight is sent after it", async () => {
    installAt("9.9.9");
    const sent: AgentStatus_State[] = [];
    const gate = Promise.withResolvers<void>();
    let first = true;
    const m = manager(goodPackage(), {
      report: async (agents) => {
        sent.push(agents[0].state);
        if (first) {
          first = false;
          await gate.promise;
        }
      },
    });
    await m.start();
    m.want(wanted());
    await waitFor(() => state(m) === AgentStatus_State.NOT_INSTALLED, "uninstalled");
    expect(sent).toEqual([AgentStatus_State.UNINSTALLING]);
    gate.resolve();
    await waitFor(() => sent.length === 2, "the latest state after the one in flight");
    expect(sent[1]).toBe(AgentStatus_State.NOT_INSTALLED);
  });
});

describe("retries", () => {
  test("a FAILED install is retried only for a newer attempt, never for the same wish again", async () => {
    const data = goodPackage();
    body = tarball([{ name: "package/claude", data: "tampered", mode: 0o755 }]);
    const m = manager(data);
    await m.start();
    m.want(wanted("9.9.9", 3n));
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED");
    expect(gets).toBe(1);
    body = data;
    // A reconnect: its AgentsWanted carries the same attempt, as does any
    // later one until the user asks again; nothing retries by itself.
    m.forAttach();
    m.ready();
    m.want(wanted("9.9.9", 3n));
    m.want(wanted("9.9.9", 3n));
    // An older attempt (never sent, but never a retry either).
    m.want(wanted("9.9.9", 2n));
    await new Promise((done) => setTimeout(done, 150));
    expect(gets).toBe(1);
    expect(state(m)).toBe(AgentStatus_State.FAILED);
    // The user asks again: a newer attempt.
    m.want(wanted("9.9.9", 4n));
    await waitFor(() => state(m) === AgentStatus_State.INSTALLED, "INSTALLED on the user's request");
    expect(gets).toBe(2);
  });

  test("a user's retry that replaces the Attach's AgentsWanted in the hint queue still retries", async () => {
    const data = goodPackage();
    body = tarball([{ name: "package/claude", data: "tampered", mode: 0o755 }]);
    const m = manager(data);
    await m.start();
    m.want(wanted("9.9.9", 1n));
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED");
    body = data;
    // After a reconnect, the Attach's AgentsWanted (attempt 1) and the
    // user's retry (attempt 2) both arrive before the pool reads either:
    // the queue keeps the latest only.
    const queue = new HintQueue();
    const event = (attempt: bigint) => create(WorkerEventSchema, { event: { case: "agentsWanted", value: wanted("9.9.9", attempt) } });
    queue.push(event(1n));
    queue.push(event(2n));
    m.forAttach();
    m.ready();
    queue.ready();
    const hints = queue.iterate()[Symbol.asyncIterator]();
    const hint = await hints.next();
    expect(hint.done).toBe(false);
    const delivered = hint.value.event;
    expect(delivered.case).toBe("agentsWanted");
    if (delivered.case === "agentsWanted") m.want(delivered.value);
    await waitFor(() => state(m) === AgentStatus_State.INSTALLED, "INSTALLED on the coalesced retry");
    queue.close();
  });

  test("a retry asked while offline arrives with the first AgentsWanted after the reconnect, and is installed", async () => {
    const data = goodPackage();
    body = tarball([{ name: "package/claude", data: "tampered", mode: 0o755 }]);
    const m = manager(data);
    await m.start();
    m.want(wanted("9.9.9", 1n));
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED");
    body = data;
    // Offline meanwhile, the user asked again (the attempt is now 2); the
    // reconnect's Attach brings it.
    m.forAttach();
    m.ready();
    m.want(wanted("9.9.9", 2n));
    await waitFor(() => state(m) === AgentStatus_State.INSTALLED, "INSTALLED after the reconnect");
    expect(gets).toBe(2);
  });

  test("a failed install removes the previous version's files", async () => {
    installAt("1.0.0");
    body = tarball([{ name: "package/claude", data: "tampered", mode: 0o755 }]);
    const m = manager(goodPackage());
    await m.start();
    expect(state(m)).toBe(AgentStatus_State.OUTDATED);
    m.want(wanted("9.9.9"));
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED");
    expect(m.statuses()[0].installedVersion).toBe("");
    expect(existsSync(join(agentDir(), "1.0.0"))).toBe(false);
  });

  test.skipIf(process.getuid?.() === 0)("a failed uninstall is retried with backoff until it succeeds", async () => {
    installAt("9.9.9");
    const versionDir = join(agentDir(), "9.9.9");
    chmodSync(versionDir, 0o555); // Its files cannot be removed.
    const log = captureLogger();
    const failures = () => log.lines.filter((line) => line.message === "agent uninstall failed; retrying").length;
    const m = manager(goodPackage(), { log });
    await m.start();
    m.want(wanted());
    await waitFor(() => state(m) === AgentStatus_State.FAILED, "FAILED uninstall");
    expect(existsSync(join(versionDir, "claude"))).toBe(true);
    // A new Attach's AgentsWanted does not stop the retries (it tries at
    // once, and fails again); only the backoff's retry can succeed after.
    // The retries run every 20 to 40 ms, so the state is sampled only
    // after a failure that followed the new AgentsWanted.
    const before = failures();
    m.ready();
    m.want(wanted());
    await waitFor(() => failures() > before && state(m) === AgentStatus_State.FAILED, "FAILED again");
    chmodSync(versionDir, 0o755);
    await waitFor(() => state(m) === AgentStatus_State.NOT_INSTALLED, "NOT_INSTALLED after a retry");
    expect(existsSync(agentDir())).toBe(false);
  });
});
