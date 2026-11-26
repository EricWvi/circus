import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test } from "vitest";
import { stateDirectory } from "../src/process.js";
import { run } from "../src/run.js";

// run() is what dist/main.js runs: these check that it clears the reason
// of an earlier terminal stop when it starts and records the reason of a
// terminal stop before it returns.
let dir = "";
let saved: NodeJS.ProcessEnv;
let stop = "";

beforeEach(() => {
  saved = { ...process.env };
  dir = mkdtempSync(join(tmpdir(), "circus-run-"));
  mkdirSync(join(dir, "home"));
  stop = join(dir, "home", "worker-state", "stop.json");
  for (const key of Object.keys(process.env)) {
    if (key.startsWith("CIRCUS_") || key.startsWith("XDG_")) delete process.env[key];
  }
  process.env.CIRCUS_HOME = join(dir, "home");
  process.env.CIRCUS_WORKER_STOP_FILE = stop;
  // The worker never keeps its state under HOME (stateDirectory skips any
  // candidate inside it). The test's own HOME keeps the test's temporary
  // directory outside it wherever TMPDIR lies, so the runtime directory
  // the last test sets is the one the worker takes.
  mkdirSync(join(dir, "user"));
  process.env.HOME = join(dir, "user");
});

afterEach(() => {
  process.env = saved;
  rmSync(dir, { recursive: true, force: true });
});

function credential() {
  mkdirSync(join(dir, "home", "config"), { mode: 0o700 });
  writeFileSync(join(dir, "home", "config", "worker.json"), JSON.stringify({
    control_plane_url: "https://127.0.0.1:1", role: "worker", principal_id: "worker", token: "A".repeat(43),
  }), { mode: 0o600 });
}

test("a terminal stop records its reason, privately", async () => {
  const status = await run();
  expect(status).toBe(process.platform === "darwin" ? 0 : 78);
  expect(JSON.parse(readFileSync(stop, "utf8"))).toEqual({
    reason: "credential", message: "worker credential missing; run circus join --role worker",
  });
  expect(statSync(stop).mode & 0o777).toBe(0o600);
});

test("an invalid setting records the config reason", async () => {
  credential();
  process.env.CIRCUS_WORKER_MAX_SESSIONS = "0";
  expect(await run()).toBe(process.platform === "darwin" ? 0 : 78);
  expect(JSON.parse(readFileSync(stop, "utf8")).reason).toBe("config");
});

test("the worker clears an earlier stop when it starts, and a failure that is retried records none", async () => {
  credential();
  mkdirSync(join(dir, "home", "worker-state"), { mode: 0o700 });
  writeFileSync(stop, JSON.stringify({ reason: "credential-refused", message: "stale" }), { mode: 0o600 });
  // A state directory that can't be made fails the start with a plain error.
  const notADirectory = join(dir, "runtime");
  writeFileSync(notADirectory, "");
  process.env.XDG_RUNTIME_DIR = notADirectory;
  expect(stateDirectory()).toBe(join(notADirectory, `circus-worker-${process.getuid?.() ?? 0}`));
  expect(await run()).toBe(1);
  expect(existsSync(stop)).toBe(false);
});
