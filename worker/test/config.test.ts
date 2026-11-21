import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { afterEach, beforeEach, expect, it } from "vitest";
import { loadConfig } from "../src/config.js";

const temp = join(import.meta.dirname, "../../.tmp");
let dir: string;
const credential = {
  control_plane_url: "https://control.example/base",
  role: "worker",
  principal_id: "worker-id",
  token: "A".repeat(43),
};

beforeEach(() => {
  mkdirSync(temp, { recursive: true });
  dir = mkdtempSync(join(temp, "worker-config-"));
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

function write(parent: string, extra = {}) {
  mkdirSync(parent, { recursive: true });
  const path = join(parent, "worker.json");
  writeFileSync(path, JSON.stringify({ ...credential, ...extra }), { mode: 0o600 });
  return path;
}

it("reads what circus join writes under CIRCUS_HOME", () => {
  const caFile = join(dir, "bundle.pem");
  write(join(dir, "config"), { ca_file: caFile });
  expect(loadConfig({
    CIRCUS_HOME: dir,
    XDG_CONFIG_HOME: join(dir, "unused"),
    CIRCUS_WORKER_MAX_SESSIONS: "64",
    CIRCUS_WORKER_LOG_LEVEL: "debug",
  })).toEqual({
    url: credential.control_plane_url, principalId: credential.principal_id, token: credential.token,
    caFile, maxSessions: 64, logLevel: "debug",
  });
});

it("uses XDG and checks permissions and worker settings", () => {
  const path = write(join(dir, "circus"));
  const env = { XDG_CONFIG_HOME: dir };
  expect(loadConfig(env)).toMatchObject({ maxSessions: 4, logLevel: "info" });
  for (const value of ["0", "65", "4.5", "1e1"]) {
    expect(() => loadConfig({ ...env, CIRCUS_WORKER_MAX_SESSIONS: value })).toThrow("CIRCUS_WORKER_MAX_SESSIONS");
  }
  expect(() => loadConfig({ ...env, CIRCUS_WORKER_LOG_LEVEL: "verbose" })).toThrow("CIRCUS_WORKER_LOG_LEVEL");
  chmodSync(path, 0o640);
  expect(() => loadConfig(env)).toThrow("chmod 600");
});

it("suggests enrollment when missing and rejects another role or a relative CA", () => {
  const env = { CIRCUS_HOME: dir };
  expect(() => loadConfig(env)).toThrow("circus join --role worker");
  const parent = join(dir, "config");
  write(parent, { role: "daemon" });
  expect(() => loadConfig(env)).toThrow("invalid worker credential fields");
  write(parent, { ca_file: "relative.pem" });
  expect(() => loadConfig(env)).toThrow("invalid worker credential fields");
});
