import { randomUUID } from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, describe, expect, test } from "vitest";
import { processStart } from "../src/agents/lock.js";
import { CliProcesses, resumeDirectory } from "../src/process.js";
import { captureLogger, waitFor } from "./runner/harness.js";

// CliProcesses where /proc is missing (macOS, the default there): no start
// times or boot id, so a live pid always counts as the recorded process and
// nothing is killed on a record's word alone. These run on every platform
// with linux = false; on macOS that is the worker's own configuration.

const alive = (pid: number) => {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
};
const killQuietly = (pid: number) => {
  try {
    process.kill(pid, "SIGKILL");
  } catch {
    // Gone.
  }
};
/** A pid above every system's limit (macOS: 99999, Linux: 2^22): never running. */
const deadPid = 2 ** 22 + 7;

const cleanups: (() => void)[] = [];
afterEach(() => {
  for (const cleanup of cleanups.splice(0).reverse()) cleanup();
});

function tempDir(prefix: string): string {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function resumeDir(): string {
  const dir = join(tmpdir(), `claude-resume-${randomUUID()}`);
  mkdirSync(dir);
  writeFileSync(join(dir, ".credentials.json"), "{}");
  cleanups.push(() => rmSync(dir, { recursive: true, force: true }));
  return dir;
}

/** A process that runs until the test ends, as another worker or CLI. */
function sleeper(): ChildProcess {
  const child = spawn("sleep", ["30"], { stdio: "ignore", detached: true });
  cleanups.push(() => killQuietly(child.pid!));
  return child;
}

const exited = (child: ChildProcess) => child.exitCode !== null || child.signalCode !== null;

function records(dir: string): string[] {
  return readdirSync(dir).filter((name) => name.endsWith(".cli.json"));
}

describe("CLI processes without /proc", () => {
  test("a CLI is recorded without start times or boot id, and its own worker's record is cleaned up after it", async () => {
    const dir = tempDir("circus-cli-");
    const resume = resumeDir();
    const processes = new CliProcesses(dir, undefined, captureLogger(), false, 100);
    const child = processes.spawner("s1")({
      command: process.execPath, args: ["-e", "setInterval(() => {}, 1000)"],
      env: { ...process.env, CLAUDE_CONFIG_DIR: resume }, signal: new AbortController().signal,
    }) as unknown as ChildProcess;
    cleanups.push(() => killQuietly(-child.pid!));
    await waitFor(() => records(dir).length === 1, "record");
    const record = JSON.parse(readFileSync(join(dir, records(dir)[0]), "utf8"));
    expect(record).toMatchObject({ sessionId: "s1", pid: child.pid, start: "", boot: "", worker: { pid: process.pid, start: "" } });
    expect(record.resumeDir?.dir).toBe(resume);
    // It does not exit: confirmExit kills its process group after the grace.
    await processes.confirmExit("s1");
    expect(child.signalCode).toBe("SIGKILL");
    // No exit time is recorded without /proc.
    expect(JSON.parse(readFileSync(join(dir, records(dir)[0]), "utf8"))).not.toHaveProperty("exited");
    processes.cleanup("s1");
    expect(records(dir)).toEqual([]);
    expect(existsSync(resume)).toBe(false);
  });

  test("a live worker pid keeps its records, its CLIs and their resume dirs", async () => {
    const dir = tempDir("circus-cli-");
    const resume = resumeDir();
    const worker = sleeper();
    const cli = sleeper();
    const name = `s1.${worker.pid}.${cli.pid}.cli.json`;
    writeFileSync(join(dir, name), JSON.stringify({
      sessionId: "s1", pid: cli.pid, start: "", boot: "", worker: { pid: worker.pid, start: "" },
      resumeDir: resumeDirectory({ CLAUDE_CONFIG_DIR: resume }, undefined),
    }));
    new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
    expect(records(dir)).toEqual([name]);
    expect(existsSync(resume)).toBe(true);
    expect(alive(cli.pid!)).toBe(true);
  });

  test("a gone worker's CLI that still runs is never killed: its record and resume dir stay until it exits", async () => {
    const dir = tempDir("circus-cli-");
    const resume = resumeDir();
    const cli = sleeper();
    const name = `s1.${deadPid}.${cli.pid}.cli.json`;
    writeFileSync(join(dir, name), JSON.stringify({
      sessionId: "s1", pid: cli.pid, start: "", boot: "", worker: { pid: deadPid, start: "" },
      resumeDir: resumeDirectory({ CLAUDE_CONFIG_DIR: resume }, undefined),
    }));
    // Without a start time the pid may be another process by now: it is
    // left running, and the record kept for a later cleanup.
    new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(alive(cli.pid!)).toBe(true);
    expect(records(dir)).toEqual([name]);
    expect(existsSync(resume)).toBe(true);
    cli.kill("SIGKILL");
    await waitFor(() => exited(cli), "CLI gone");
    new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
    expect(records(dir)).toEqual([]);
    expect(existsSync(resume)).toBe(false);
  });

  test("a record with Linux start times is cleaned up without killing anything", async () => {
    const dir = tempDir("circus-cli-");
    const cli = sleeper();
    writeFileSync(join(dir, `s1.${deadPid}.${cli.pid}.cli.json`), JSON.stringify({
      sessionId: "s1", pid: cli.pid, start: "12345", boot: "", worker: { pid: deadPid, start: "1" },
    }));
    new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(alive(cli.pid!)).toBe(true);
    expect(records(dir)).toEqual([]);
  });

  test("unreadable and foreign records are skipped", () => {
    const dir = tempDir("circus-cli-");
    writeFileSync(join(dir, "broken.cli.json"), "{");
    writeFileSync(join(dir, "other.json"), "{}");
    new CliProcesses(dir, undefined, captureLogger(), false).cleanupAll();
    expect(readdirSync(dir).sort()).toEqual(["broken.cli.json", "other.json"]);
  });
});

describe("process start times", () => {
  test("are stable for a running process, and unknown for one that is gone", async () => {
    const child = sleeper();
    const start = processStart(child.pid!);
    if (process.platform === "linux" || process.platform === "darwin") expect(start).toBeTruthy();
    expect(processStart(child.pid!)).toBe(start);
    child.kill("SIGKILL");
    await waitFor(() => exited(child), "process gone");
    expect(processStart(child.pid!)).toBeUndefined();
    expect(processStart(deadPid)).toBeUndefined();
  });
});
