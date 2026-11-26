import { existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, expect, test } from "vitest";
import { registryBase } from "../src/agents/install.js";
import { ConfigError, loadConfig } from "../src/config.js";
import { exitStatus } from "../src/exit.js";
import { clearStop, recordStop, stopFile, stopReason } from "../src/stop.js";
import { TerminalLinkError } from "../src/wire/connection.js";

const dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

test("a refused credential or a bad configuration stops the worker for good", () => {
  for (const error of [new TerminalLinkError("worker upgrade HTTP 401"), new ConfigError("worker credential missing", "credential")]) {
    expect(exitStatus(error, "linux")).toBe(78);
    expect(exitStatus(error, "darwin")).toBe(0);
  }
});

test("other failures are restarted", () => {
  for (const error of [new Error("socket hang up"), "thrown string", undefined]) {
    expect(exitStatus(error, "linux")).toBe(1);
    expect(exitStatus(error, "darwin")).toBe(1);
    expect(stopReason(error)).toBeUndefined();
  }
});

test("every configuration failure is a ConfigError of its kind", () => {
  const env = { CIRCUS_HOME: "/nonexistent/circus-home" };
  expect(() => loadConfig(env)).toThrow(ConfigError);
  expect(() => loadConfig(env)).toThrow("circus join --role worker");
  try {
    loadConfig(env);
  } catch (error) {
    expect(stopReason(error)).toBe("credential");
  }
});

test("an invalid CIRCUS_NPM_REGISTRY is a configuration error", () => {
  for (const value of ["ftp://registry.example", "not a url"]) {
    let thrown: unknown;
    try {
      registryBase({ CIRCUS_NPM_REGISTRY: value });
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(ConfigError);
    expect(stopReason(thrown)).toBe("config");
    expect(exitStatus(thrown, "linux")).toBe(78);
  }
  expect(registryBase({ CIRCUS_NPM_REGISTRY: "https://mirror.example/npm/" })).toBe("https://mirror.example/npm");
});

test("the reasons of terminal stops", () => {
  expect(stopReason(new TerminalLinkError("worker upgrade HTTP 403: the control plane refused the credential"))).toBe("credential-refused");
  expect(stopReason(new TerminalLinkError("incompatible worker Welcome", "incompatible"))).toBe("incompatible");
  expect(stopReason(new ConfigError("CIRCUS_WORKER_LOG_LEVEL must be debug, info, warn or error", "config"))).toBe("config");
});

test("the stop file is the recorded one, written atomically and privately, under an existing directory only", () => {
  const home = mkdtempSync(join(tmpdir(), "circus-stop-"));
  dirs.push(home);
  const file = join(home, "circus-home", "worker-state", "stop.json");
  const env = { CIRCUS_HOME: join(home, "elsewhere"), CIRCUS_WORKER_STOP_FILE: file };
  expect(stopFile(env)).toBe(file);
  // $CIRCUS_HOME itself is never created.
  recordStop("config", "bad", env);
  expect(existsSync(join(home, "circus-home"))).toBe(false);
  mkdirSync(join(home, "circus-home"), { mode: 0o755 });
  recordStop("credential-refused", "worker upgrade HTTP 401: the control plane refused the credential", env);
  expect(JSON.parse(readFileSync(file, "utf8"))).toEqual({
    reason: "credential-refused", message: "worker upgrade HTTP 401: the control plane refused the credential",
  });
  expect(statSync(file).mode & 0o777).toBe(0o600);
  expect(statSync(join(home, "circus-home", "worker-state")).mode & 0o777).toBe(0o700);
  expect(readdirSync(join(home, "circus-home", "worker-state"))).toEqual(["stop.json"]);
  clearStop(env);
  expect(existsSync(file)).toBe(false);
  // Without a recorded path: the data directory's.
  expect(stopFile({ XDG_DATA_HOME: "/data" })).toBe("/data/circus/worker-state/stop.json");
  expect(stopFile({ CIRCUS_HOME: "/srv/circus", CIRCUS_WORKER_STOP_FILE: "relative/stop.json" })).toBe("/srv/circus/worker-state/stop.json");
});
