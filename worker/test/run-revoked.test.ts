import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { run } from "../src/run.js";
import { recordStop } from "../src/stop.js";
import { TerminalLinkError } from "../src/wire/connection.js";

// The control plane's proof that it revoked this worker's own identity,
// thrown once the worker has read its credential.
vi.mock("../src/process.js", async (importOriginal) => ({
  ...await importOriginal<typeof import("../src/process.js")>(),
  ensureStateDirectory: () => {
    throw new TerminalLinkError("worker upgrade HTTP 403: the control plane revoked the credential (worker worker)", "refused", "worker");
  },
}));

let dir = "";
let saved: NodeJS.ProcessEnv;

beforeEach(() => {
  saved = { ...process.env };
  dir = mkdtempSync(join(tmpdir(), "circus-run-revoked-"));
  for (const key of Object.keys(process.env)) {
    if (key.startsWith("CIRCUS_") || key.startsWith("XDG_")) delete process.env[key];
  }
  process.env.CIRCUS_HOME = join(dir, "home");
  mkdirSync(join(dir, "home", "config"), { recursive: true, mode: 0o700 });
  writeFileSync(join(dir, "home", "config", "worker.json"), JSON.stringify({
    control_plane_url: "https://127.0.0.1:1", role: "worker", principal_id: "worker", token: "A".repeat(43),
  }), { mode: 0o600 });
  // A stand-in for circus: it records its arguments and exits with $STATUS.
  const circus = join(dir, "circus");
  writeFileSync(circus, `#!/bin/sh\nprintf '%s\\n' "$@" > "${join(dir, "args")}"\nexit "$STATUS"\n`, { mode: 0o755 });
  process.env.CIRCUS_BIN = circus;
});

afterEach(() => {
  process.env = saved;
  vi.restoreAllMocks();
  rmSync(dir, { recursive: true, force: true });
});

function logged(write: { mock: { calls: unknown[][] } }): Record<string, unknown>[] {
  return write.mock.calls.map((call) => JSON.parse(String(call[0])) as Record<string, unknown>);
}

test("a stop reason that cannot be recorded skips neither the credential removal nor the terminal status", async () => {
  // The stop file lies under a regular file: recording it fails.
  const regular = join(dir, "regular");
  writeFileSync(regular, "");
  process.env.CIRCUS_WORKER_STOP_FILE = join(regular, "stop.json");
  expect(() => recordStop("credential-refused", "probe")).toThrow();
  process.env.STATUS = "0";
  const write = vi.spyOn(process.stderr, "write").mockReturnValue(true);
  expect(await run([])).toBe(process.platform === "darwin" ? 0 : 78);
  expect(readFileSync(join(dir, "args"), "utf8")).toContain("--credential-only");
  expect(logged(write)).toEqual(expect.arrayContaining([
    expect.objectContaining({ message: "could not record the stop reason" }),
    expect.objectContaining({ message: "worker credential after the revocation", service: false, outcome: "removed" }),
  ]));
});

test("a credential file that is already gone is logged as gone, not kept", async () => {
  process.env.CIRCUS_WORKER_STOP_FILE = join(dir, "state", "stop.json");
  process.env.STATUS = "3";
  const write = vi.spyOn(process.stderr, "write").mockReturnValue(true);
  expect(await run([])).toBe(process.platform === "darwin" ? 0 : 78);
  expect(existsSync(join(dir, "state", "stop.json"))).toBe(true);
  expect(logged(write)).toEqual(expect.arrayContaining([
    expect.objectContaining({ message: "worker credential after the revocation", outcome: "gone" }),
  ]));
});
