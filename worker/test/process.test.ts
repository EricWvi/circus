import { randomUUID } from "node:crypto";
import { once } from "node:events";
import { lstatSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, renameSync, rmSync, statSync, writeFileSync, existsSync, chmodSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, type ChildProcess } from "node:child_process";
import { describe, expect, test, vi } from "vitest";
import { CliProcesses, ensureStateDirectory, findExecGuard, resumeDirectory, stateDirectory } from "../src/process.js";
import { captureLogger, waitFor } from "./runner/harness.js";

const standin = fileURLToPath(new URL("./fixtures/exec-guard-standin.sh", import.meta.url));
const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
};
const linux = process.platform === "linux";
const killQuietly = (pid: number) => {
  try {
    process.kill(pid, "SIGKILL");
  } catch {
    // Gone.
  }
};

function spawnCli(processes: CliProcesses, id: string, script: string, env: NodeJS.ProcessEnv = process.env): ChildProcess {
  const child = processes.spawner(id)({ command: process.execPath, args: ["-e", script], env, signal: new AbortController().signal }) as unknown as ChildProcess;
  return child;
}

describe("state directory", () => {
  test("uses XDG_RUNTIME_DIR, else TMPDIR, never a directory under HOME", () => {
    const uid = process.getuid!();
    expect(stateDirectory({ XDG_RUNTIME_DIR: "/run/user/1", TMPDIR: "/var/tmp" }, "/home/u")).toBe(`/run/user/1/circus-worker-${uid}`);
    expect(stateDirectory({ TMPDIR: "/var/tmp" }, "/home/u")).toBe(`/var/tmp/circus-worker-${uid}`);
    expect(stateDirectory({ XDG_RUNTIME_DIR: "/home/u/run", TMPDIR: "/home/u/tmp" }, "/home/u")).not.toMatch(/^\/home\/u/);
  });

  test("is created 0700 and must not be accessible to others", () => {
    const base = mkdtempSync(join(tmpdir(), "circus-state-"));
    const dir = join(base, "state");
    ensureStateDirectory(dir);
    expect(statSync(dir).mode & 0o777).toBe(0o700);
    ensureStateDirectory(dir);
    const open = join(base, "open");
    mkdirSync(open, { mode: 0o755 });
    chmodSync(open, 0o755);
    expect(() => ensureStateDirectory(open)).toThrow(/0700/);
  });
});

describe.skipIf(!linux)("CLI processes", () => {
  test("the exec guard is found when it runs, and not otherwise", async () => {
    expect(await findExecGuard({ ...process.env, CIRCUS_BIN: standin })).toBe(standin);
    expect(await findExecGuard({ ...process.env, CIRCUS_BIN: "/bin/false" })).toBeUndefined();
    expect(await findExecGuard({ ...process.env, CIRCUS_BIN: "/nonexistent/circus" })).toBeUndefined();
  });

  test("the CLI is launched through the guard and the guard is recorded", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const log = join(dir, "argv.log");
    const processes = new CliProcesses(dir, standin, captureLogger());
    const child = spawnCli(processes, "s1", "process.stdin.pipe(process.stdout)", { ...process.env, STANDIN_LOG: log });
    child.stdin!.write("ping\n");
    const echoed = await new Promise<string>((resolve) => child.stdout!.once("data", (d) => resolve(String(d))));
    expect(echoed).toBe("ping\n");
    expect(readFileSync(log, "utf8")).toBe(`internal exec-guard --parent ${process.pid} -- ${process.execPath} -e process.stdin.pipe(process.stdout)\n`);
    expect(readFileSync(`/proc/${child.pid}/cmdline`, "utf8").split("\0")).toContain(standin);
    const file = join(dir, `s1.${process.pid}.${child.pid}.cli.json`);
    const record = JSON.parse(readFileSync(file, "utf8"));
    expect(record).toMatchObject({ sessionId: "s1", pid: child.pid, guarded: true, worker: { pid: process.pid }, boot: readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim() });
    expect(record.start).toMatch(/^\d+$/);
    expect(record.worker.start).toMatch(/^\d+$/);
    child.stdin!.end();
    await processes.confirmExit("s1");
    expect(child.exitCode).toBe(0);
    expect(JSON.parse(readFileSync(file, "utf8")).exited).toMatch(/^\d+$/);
    processes.cleanup("s1");
    expect(existsSync(file)).toBe(false);
  });

  test.each(["exit", "stall"])("a guard that will %s receives SIGUSR1 before any group kill", async (mode) => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const signalLog = join(dir, "signals.log");
    const log = captureLogger();
    const processes = new CliProcesses(dir, standin, log, true, 50);
    const child = spawnCli(processes, "s1", "", { ...process.env, STANDIN_LOG: signalLog, STANDIN_USR1: mode });
    const kill = vi.spyOn(process, "kill");
    try {
      await new Promise<void>((resolve) => child.stdout!.once("data", () => resolve()));
      if (mode === "stall") {
        vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
        const signalled = once(child.stdout!, "data");
        const exited = processes.confirmExit("s1");
        await vi.advanceTimersByTimeAsync(50);
        expect(kill.mock.calls).toEqual([[child.pid, "SIGUSR1"]]);
        expect(String((await signalled)[0])).toBe("USR1\n");
        await vi.advanceTimersByTimeAsync(6_999);
        expect(kill.mock.calls).toEqual([[child.pid, "SIGUSR1"]]);
        await vi.advanceTimersByTimeAsync(1);
        expect(kill.mock.calls).toEqual([[child.pid, "SIGUSR1"], [-child.pid!, "SIGKILL"]]);
        await exited;
      } else {
        await processes.confirmExit("s1");
      }
      expect(readFileSync(signalLog, "utf8")).toMatch(/\nUSR1\n$/);
      if (mode === "exit") {
        expect(child.exitCode).toBe(137);
        expect(kill.mock.calls).toEqual([[child.pid, "SIGUSR1"]]);
        expect(log.lines.some(({ message }) => message === "exec-guard exited after SIGUSR1")).toBe(true);
      } else {
        expect(child.signalCode).toBe("SIGKILL");
        expect(log.lines.some(({ message }) => message === "exec-guard exited after process-group SIGKILL")).toBe(true);
      }
    } finally {
      vi.useRealTimers();
      kill.mockRestore();
      if (child.exitCode === null && child.signalCode === null && child.pid !== undefined) killQuietly(-child.pid);
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a failed CLI's stderr tail includes what is written after its exit, until stderr ends", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const log = captureLogger();
    const processes = new CliProcesses(dir, undefined, log);
    // A descendant keeps stderr open and writes to it after the CLI exited.
    const script = "require('child_process').spawn('sh', ['-c', 'sleep 0.2; echo late-line >&2'], { stdio: ['ignore', 'ignore', 'inherit'] }).unref(); " +
      "console.error('early-line'); process.exit(3)";
    spawnCli(processes, "s1", script);
    try {
      await waitFor(() => log.lines.some((l) => l.message === "Claude Code exited"), "the exit logged", 3_000);
      const line = log.lines.find((l) => l.message === "Claude Code exited")!;
      expect(line.fields).toMatchObject({ code: 3 });
      expect(String(line.fields?.stderr)).toContain("early-line");
      expect(String(line.fields?.stderr)).toContain("late-line");
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("flush logs an exited CLI's tail while a descendant still holds its stderr, once", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const log = captureLogger();
    const processes = new CliProcesses(dir, undefined, log);
    const script = "require('child_process').spawn('sh', ['-c', 'sleep 0.4; echo late-line >&2'], { stdio: ['ignore', 'ignore', 'inherit'] }).unref(); " +
      "console.error('early-line'); process.exit(3)";
    spawnCli(processes, "s1", script);
    const exitLines = () => log.lines.filter((l) => l.message === "Claude Code exited");
    try {
      await processes.confirmExit("s1");
      await waitFor(() => readFileSync(join(dir, readdirSync(dir).find((n) => n.endsWith(".cli.json"))!), "utf8").includes("exited"), "exit recorded");
      // stderr is still open: nothing logged yet; the worker exits now.
      expect(exitLines()).toHaveLength(0);
      processes.flush();
      expect(exitLines()).toHaveLength(1);
      expect(exitLines()[0].fields).toMatchObject({ code: 3 });
      expect(String(exitLines()[0].fields?.stderr)).toContain("early-line");
      // The descendant's later output and the end of stderr log nothing more.
      await new Promise((resolve) => setTimeout(resolve, 1_200));
      expect(exitLines()).toHaveLength(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a CLI that does not exit is killed with its process group", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const processes = new CliProcesses(dir, undefined, captureLogger(), true, 100);
    const child = spawnCli(processes, "s1", "process.stdin.resume(); process.stdin.on('end', () => {}); setInterval(() => {}, 1000)");
    child.stdin!.end();
    await processes.confirmExit("s1");
    expect(child.signalCode).toBe("SIGKILL");
  });

  test("leftover processes of a released CLI are killed, matching start times", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const processes = new CliProcesses(dir, undefined, captureLogger());
    // The CLI leaves a child behind in its session and exits.
    const child = spawnCli(processes, "s1", `
      const { spawn } = require("node:child_process");
      const orphan = spawn("sleep", ["30"], { stdio: "ignore" });
      process.stdout.write(String(orphan.pid) + "\\n", () => process.exit(0));
    `);
    const orphan = Number(await new Promise<string>((resolve) => child.stdout!.once("data", (d) => resolve(String(d)))));
    await processes.confirmExit("s1");
    expect(alive(orphan)).toBe(true);
    processes.cleanup("s1");
    await waitFor(() => !alive(orphan) || readFileSync(`/proc/${orphan}/stat`, "utf8").split(") ")[1].startsWith("Z"), "orphan killed");
  });

  test("a record whose pid now leads another process group is left alone", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    // An unrelated group leader (its own session), and a record naming its pid with another start time.
    const other = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
    try {
      await waitFor(() => existsSync(`/proc/${other.pid}/stat`), "process");
      const boot = readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim();
      writeFileSync(join(dir, "old.1.cli.json"), JSON.stringify({ sessionId: "old", pid: other.pid, start: "1", boot, worker: { pid: 1, start: "0" } }));
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(alive(other.pid!)).toBe(true);
      expect(existsSync(join(dir, "old.1.cli.json"))).toBe(false);
      // The same record with the right start time is a leftover: killed.
      const start = readFileSync(`/proc/${other.pid}/stat`, "utf8").split(") ")[1].split(" ")[19];
      writeFileSync(join(dir, "old.1.cli.json"), JSON.stringify({ sessionId: "old", pid: other.pid, start, boot, worker: { pid: 1, start: "0" } }));
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await waitFor(() => other.signalCode === "SIGKILL", "leftover killed");
    } finally {
      other.kill("SIGKILL");
    }
  });

  test("a record from another boot kills nothing", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const other = spawn("sleep", ["30"], { detached: true, stdio: "ignore" });
    try {
      await waitFor(() => existsSync(`/proc/${other.pid}/stat`), "process");
      const start = readFileSync(`/proc/${other.pid}/stat`, "utf8").split(") ")[1].split(" ")[19];
      writeFileSync(join(dir, "old.1.cli.json"), JSON.stringify({ sessionId: "old", pid: other.pid, start, boot: "another-boot", worker: { pid: 1, start: "0" } }));
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(alive(other.pid!)).toBe(true);
      expect(existsSync(join(dir, "old.1.cli.json"))).toBe(false);
    } finally {
      other.kill("SIGKILL");
    }
  });

  test("a second worker's start-up cleanup leaves a live worker's CLI alone", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    // The first worker: a live process standing in for it.
    const worker = spawn("sleep", ["30"], { stdio: "ignore" });
    try {
      await waitFor(() => existsSync(`/proc/${worker.pid}/stat`), "worker");
      const workerStart = readFileSync(`/proc/${worker.pid}/stat`, "utf8").split(") ")[1].split(" ")[19];
      const first = new CliProcesses(dir, undefined, captureLogger(), true, 5_000, { pid: worker.pid!, start: workerStart });
      const child = spawnCli(first, "s1", "setInterval(() => {}, 1000)");
      await waitFor(() => existsSync(join(dir, `s1.${worker.pid}.${child.pid}.cli.json`)), "record");
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await new Promise((resolve) => setTimeout(resolve, 100));
      expect(child.signalCode).toBeNull();
      // Once that worker is gone, its CLI is a leftover.
      worker.kill("SIGKILL");
      await waitFor(() => worker.exitCode !== null || worker.signalCode !== null, "worker gone");
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await waitFor(() => child.signalCode === "SIGKILL", "leftover killed");
    } finally {
      worker.kill("SIGKILL");
    }
  });

  const resumeDir = () => {
    const dir = join(tmpdir(), `claude-resume-${randomUUID()}`);
    mkdirSync(dir);
    writeFileSync(join(dir, ".credentials.json"), "{}");
    return dir;
  };

  test("startup cleanup lets a dead worker's live guard finish teardown", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const owner = { pid: 2 ** 22 + 7, start: "1" };
    const processes = new CliProcesses(dir, standin, captureLogger(), true, 5_000, owner);
    const child = spawnCli(processes, "s1", "process.stdin.resume(); console.log('ready')");
    const record = join(dir, `s1.${owner.pid}.${child.pid}.cli.json`);
    try {
      await new Promise<void>((resolve) => child.stdout!.once("data", () => resolve()));
      const replacement = new CliProcesses(dir, standin, captureLogger());
      replacement.cleanupAll();
      expect(existsSync(record)).toBe(true);
      child.stdin!.end();
      await processes.confirmExit("s1");
      expect(child.exitCode).toBe(0);
      replacement.cleanupAll();
      expect(existsSync(record)).toBe(false);
    } finally {
      if (child.exitCode === null && child.signalCode === null && child.pid !== undefined) killQuietly(-child.pid);
      rmSync(dir, { recursive: true, force: true });
    }
  });

  test("a dead CLI's claude-resume directory is removed with its leftovers", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const resume = resumeDir();
    const processes = new CliProcesses(dir, undefined, captureLogger());
    const child = spawnCli(processes, "s1", "process.exit(0)", { ...process.env, CLAUDE_CONFIG_DIR: resume });
    await processes.confirmExit("s1");
    expect(child.exitCode).toBe(0);
    processes.cleanup("s1");
    expect(existsSync(resume)).toBe(false);
  });

  test("a config dir the SDK did not make for this CLI is never removed", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const processes = new CliProcesses(dir, undefined, captureLogger());
    // Not a resume dir; a resume dir outside the worker's tmpdir (a parent
    // session's, inherited); and the worker's own CLAUDE_CONFIG_DIR.
    const config = mkdtempSync(join(tmpdir(), "circus-config-"));
    const elsewhere = join(mkdtempSync(join(tmpdir(), "circus-parent-")), `claude-resume-${randomUUID()}`);
    mkdirSync(elsewhere);
    const own = resumeDir();
    const previous = process.env.CLAUDE_CONFIG_DIR;
    process.env.CLAUDE_CONFIG_DIR = own;
    try {
      for (const [i, configDir] of [config, elsewhere, own].entries()) {
        spawnCli(processes, `s${i}`, "process.exit(0)", { ...process.env, CLAUDE_CONFIG_DIR: configDir });
        await processes.confirmExit(`s${i}`);
        processes.cleanup(`s${i}`);
        expect(existsSync(configDir)).toBe(true);
      }
    } finally {
      if (previous === undefined) delete process.env.CLAUDE_CONFIG_DIR;
      else process.env.CLAUDE_CONFIG_DIR = previous;
    }
    // A resume dir made before the worker started is not this worker's either.
    expect(resumeDirectory({ CLAUDE_CONFIG_DIR: resumeDir() }, undefined, Date.now() + 60_000)).toBeUndefined();
    expect(resumeDirectory({ CLAUDE_CONFIG_DIR: resumeDir() }, undefined)).toBeDefined();
  });

  test("a resume dir is removed only once its CLI is gone; until then the record stays", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const resume = resumeDir();
    // A worker that is gone, whose CLI still runs.
    const dead = new CliProcesses(dir, undefined, captureLogger(), true, 5_000, { pid: 2 ** 22 + 7, start: "1" });
    const child = spawnCli(dead, "s1", "setInterval(() => {}, 1000)", { ...process.env, CLAUDE_CONFIG_DIR: resume });
    await waitFor(() => readdirSync(dir).some((n) => n.endsWith(".cli.json")), "record");
    // The kill is sent, but the CLI is not reaped before this synchronous cleanup returns.
    new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
    expect(existsSync(resume)).toBe(true);
    expect(readdirSync(dir).filter((n) => n.endsWith(".cli.json"))).toHaveLength(1);
    await waitFor(() => child.signalCode === "SIGKILL", "CLI killed");
    new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
    expect(existsSync(resume)).toBe(false);
    expect(readdirSync(dir).filter((n) => n.endsWith(".cli.json"))).toEqual([]);
  });

  test("without start times (macOS) a live worker pid keeps its records and resume dirs", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const resume = resumeDir();
    const worker = spawn("sleep", ["30"], { stdio: "ignore" });
    try {
      await waitFor(() => existsSync(`/proc/${worker.pid}`), "worker");
      const record = { sessionId: "s1", pid: 2 ** 22 + 9, start: "", boot: "", worker: { pid: worker.pid, start: "" }, resumeDir: resumeDirectory({ CLAUDE_CONFIG_DIR: resume }, undefined) };
      writeFileSync(join(dir, `s1.${worker.pid}.1.cli.json`), JSON.stringify(record));
      new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
      expect(existsSync(resume)).toBe(true);
      worker.kill("SIGKILL");
      await waitFor(() => worker.signalCode !== null, "worker gone");
      new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
      expect(existsSync(resume)).toBe(false);
    } finally {
      worker.kill("SIGKILL");
    }
  });

  test("a resume dir is removed only if it is still the directory recorded at spawn", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const resume = resumeDir();
    const processes = new CliProcesses(dir, undefined, captureLogger());
    spawnCli(processes, "s1", "process.exit(0)", { ...process.env, CLAUDE_CONFIG_DIR: resume });
    await processes.confirmExit("s1");
    // Preserve the recorded inode while replacing the directory at that path.
    const original = `${resume}-original`;
    renameSync(resume, original);
    try {
      mkdirSync(resume);
      processes.cleanup("s1");
      expect(existsSync(resume)).toBe(true);
    } finally {
      rmSync(resume, { recursive: true, force: true });
      rmSync(original, { recursive: true });
    }
  });

  test("without a birth time the resume dir is still recorded (by inode)", () => {
    // A birth time before the worker's start rejects it; none (0) does not.
    const resume = resumeDir();
    expect(resumeDirectory({ CLAUDE_CONFIG_DIR: resume }, undefined, Date.now() + 60_000)).toBeUndefined();
    const info = lstatSync(resume, { bigint: true });
    expect(resumeDirectory({ CLAUDE_CONFIG_DIR: resume }, undefined)).toEqual({ dir: resume, dev: String(info.dev), ino: String(info.ino) });
  });

  test("a restarted query's CLI gets its own record; the previous one is cleaned while the session runs", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const processes = new CliProcesses(dir, undefined, captureLogger());
    const first = spawnCli(processes, "s1", "process.exit(0)");
    await processes.confirmExit("s1");
    const second = spawnCli(processes, "s1", "setInterval(() => {}, 1000)");
    const records = () => readdirSync(dir).filter((n) => n.endsWith(".cli.json")).sort();
    expect(records()).toEqual([`s1.${process.pid}.${first.pid}.cli.json`, `s1.${process.pid}.${second.pid}.cli.json`].sort());
    processes.cleanup("s1");
    expect(records()).toEqual([`s1.${process.pid}.${second.pid}.cli.json`]);
    expect(second.signalCode).toBeNull();
    second.kill("SIGKILL");
    await processes.confirmExit("s1");
  });

  test("processes of a session that began after its leader exited are left alone (pid reuse)", async () => {
    const dir = mkdtempSync(join(tmpdir(), "circus-cli-"));
    const boot = readFileSync("/proc/sys/kernel/random/boot_id", "utf8").trim();
    // A session leader that leaves a child behind and exits.
    const leader = spawn("sh", ["-c", "sleep 30 & echo $!"], { detached: true, stdio: ["ignore", "pipe", "ignore"] });
    const member = Number(await new Promise<string>((resolve) => leader.stdout!.once("data", (d) => resolve(String(d)))));
    await new Promise((resolve) => leader.once("exit", resolve));
    try {
      const memberStart = BigInt(readFileSync(`/proc/${member}/stat`, "utf8").split(") ")[1].split(" ")[19]);
      const record = (exited?: bigint) => JSON.stringify({
        sessionId: "old", pid: leader.pid, start: "1", boot, worker: { pid: 2 ** 22 + 7, start: "1" },
        ...(exited !== undefined ? { exited: String(exited) } : {}),
      });
      // The recorded CLI exited before that process started: another session reused the pid.
      writeFileSync(join(dir, "old.1.1.cli.json"), record(memberStart - 1n));
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(alive(member)).toBe(true);
      // Without an exit time it counts as the CLI's leftover.
      writeFileSync(join(dir, "old.1.1.cli.json"), record());
      new CliProcesses(dir, undefined, captureLogger()).cleanupAll();
      await waitFor(() => !alive(member) || readFileSync(`/proc/${member}/stat`, "utf8").split(") ")[1].startsWith("Z"), "leftover killed");
    } finally {
      killQuietly(member);
    }
  });
});
