import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createHash } from "node:crypto";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test } from "vitest";
import { ConfigError } from "../src/config.js";
import { removeRevoked, SERVICE_FLAG } from "../src/rejected.js";
import { TerminalLinkError } from "../src/wire/connection.js";

let dir = "";
let env: NodeJS.ProcessEnv = {};
const own = { principalId: "worker-1", token: "T".repeat(43) };

// A stand-in for circus: it records its arguments and exits with $STATUS.
beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), "circus-rejected-"));
  const circus = join(dir, "circus");
  writeFileSync(circus, `#!/bin/sh\nprintf '%s\\n' "$@" > "${join(dir, "args")}"\necho "said $STATUS" >&2\nexit "$STATUS"\n`, { mode: 0o755 });
  env = { CIRCUS_BIN: circus, CIRCUS_HOME: join(dir, "home"), STATUS: "0", PATH: process.env.PATH };
});

afterEach(() => rmSync(dir, { recursive: true, force: true }));

const revoked = (principal: string) =>
  new TerminalLinkError(`worker upgrade HTTP 403: the control plane revoked the credential (worker ${principal})`, "refused", principal);

test("the service asks circus to remove itself when the control plane revoked its own identity", () => {
  expect(removeRevoked(revoked("worker-1"), [SERVICE_FLAG], own, env)).toEqual({ status: 0, output: "said 0" });
  // The token's hash, never the token, goes on the command line.
  const args = readFileSync(join(dir, "args"), "utf8").trim().split("\n");
  expect(args).toEqual([
    "internal", "worker-rejected", "--credentials", join(dir, "home", "config", "worker.json"), "--principal", "worker-1",
    "--token-sha256", createHash("sha256").update(own.token).digest("hex"), "--pid", String(process.pid),
  ]);
  expect(args.join(" ")).not.toContain(own.token);
  env.STATUS = "1";
  expect(removeRevoked(revoked("worker-1"), [SERVICE_FLAG], own, env)).toEqual({ status: 1, output: "said 1" });
});

test("in the foreground it asks circus to remove only the credential file", () => {
  expect(removeRevoked(revoked("worker-1"), [], own, env)).toEqual({ status: 0, output: "said 0" });
  const args = readFileSync(join(dir, "args"), "utf8").trim().split("\n");
  expect(args).toEqual([
    "internal", "worker-rejected", "--credentials", join(dir, "home", "config", "worker.json"), "--principal", "worker-1",
    "--token-sha256", createHash("sha256").update(own.token).digest("hex"), "--credential-only",
  ]);
  expect(args.join(" ")).not.toContain(own.token);
  env.STATUS = "1";
  expect(removeRevoked(revoked("worker-1"), ["--other"], own, env)).toEqual({ status: 1, output: "said 1" });
});

test("nothing is removed for another identity's revocation, an unknown credential or for any other stop", () => {
  expect(removeRevoked(revoked("worker-2"), [SERVICE_FLAG], own, env)).toMatchObject({ status: null });
  expect(removeRevoked(revoked("worker-1"), [SERVICE_FLAG], undefined, env)).toMatchObject({ status: null });
  const others: unknown[] = [
    // An unknown credential (the control plane may run on another database).
    new TerminalLinkError("worker upgrade HTTP 401: the control plane refused the credential"),
    new TerminalLinkError("worker Hello rejected (code 4)"),
    new TerminalLinkError("incompatible worker Welcome", "incompatible"),
    new ConfigError("worker credential missing", "credential"),
    new Error("worker upgrade HTTP 503"),
  ];
  for (const error of others) {
    expect(removeRevoked(error, [SERVICE_FLAG], own, env)).toBeUndefined();
    expect(removeRevoked(error, [], own, env)).toBeUndefined();
  }
  expect(removeRevoked(revoked("worker-2"), [], own, env)).toMatchObject({ status: null });
  expect(() => readFileSync(join(dir, "args"))).toThrow();
});
