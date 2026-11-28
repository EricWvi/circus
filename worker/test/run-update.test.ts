import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { run } from "../src/run.js";
import { SERVICE_FLAG } from "../src/rejected.js";
import { FEATURE_MACHINE_UPDATE } from "../src/update.js";
import { serve } from "./peer.js";

// run() with a test control plane: UpdateMachine runs `circus update` only
// in the installed service; a foreground worker neither lists the feature
// nor runs the command it gets anyway.
let dir = "";
let saved: NodeJS.ProcessEnv;

beforeEach(() => {
  saved = { ...process.env };
  dir = mkdtempSync(join(tmpdir(), "circus-run-update-"));
  for (const key of Object.keys(process.env)) {
    if (key.startsWith("CIRCUS_") || key.startsWith("XDG_")) delete process.env[key];
  }
  process.env.CIRCUS_HOME = join(dir, "home");
  mkdirSync(join(dir, "user"));
  process.env.HOME = join(dir, "user");
  // A stand-in for circus that records `update` runs only.
  const circus = join(dir, "circus");
  writeFileSync(circus, `#!/bin/sh\n[ "$1" = update ] && echo ran >> "${join(dir, "updates")}"\nexit 1\n`, { mode: 0o755 });
  process.env.CIRCUS_BIN = circus;
});

afterEach(() => {
  process.env = saved;
  vi.restoreAllMocks();
  rmSync(dir, { recursive: true, force: true });
});

function logged(write: { mock: { calls: unknown[][] } }): { message?: string }[] {
  return write.mock.calls.flatMap((call) => {
    try {
      return [JSON.parse(String(call[0])) as { message?: string }];
    } catch {
      return [];
    }
  });
}

for (const service of [true, false]) {
  test(`UpdateMachine ${service ? "runs circus update in the service" : "is ignored in the foreground"}`, async () => {
    const server = await serve();
    let done: Promise<number> | undefined;
    let ended = false;
    try {
      mkdirSync(join(dir, "home", "config"), { recursive: true, mode: 0o700 });
      writeFileSync(join(dir, "home", "config", "worker.json"), JSON.stringify({
        control_plane_url: server.url, role: "worker", principal_id: "worker", token: "A".repeat(43),
      }), { mode: 0o600 });
      const write = vi.spyOn(process.stderr, "write").mockReturnValue(true);
      done = run(service ? [SERVICE_FLAG] : []);
      const running = done;
      const peer = await Promise.race([server.peers.take(), running.then((status) => {
        throw new Error(`run ended with ${status}: ${JSON.stringify(logged(write))}`);
      })]);
      const next = async () => {
        let m = await peer.receive();
        while (m.payload.case === "heartbeat") m = await peer.receive();
        return m;
      };
      const hello = await next();
      expect(hello.payload.case).toBe("hello");
      expect(hello.payload.case === "hello" && hello.payload.value.features).toEqual(service ? [FEATURE_MACHINE_UPDATE] : []);
      peer.welcome({ heartbeatIntervalMs: 60_000, heartbeatTimeoutMs: 180_000 });
      const attach = await next();
      expect(attach.payload.case).toBe("attach");
      // The link is ready (hints flow) once ResumeLeases is answered.
      peer.resume(await next(), []);
      // Sent anyway: the worker must not trust the control plane to check.
      peer.send({ case: "workerEvent", value: { event: { case: "updateMachine", value: { updateId: "u1", version: "v9.9.9" } } } },
        attach.requestId);
      if (service) {
        // The fake fails: its report arrives.
        let report = await next();
        while (report.payload.case !== "updateFailed") report = await next();
        expect(report.payload.value.updateId).toBe("u1");
        peer.ok(report.requestId);
        expect(readFileSync(join(dir, "updates"), "utf8")).toBe("ran\n");
      } else {
        await expect.poll(() => logged(write).some((r) => r.message?.startsWith("UpdateMachine ignored")), { timeout: 3_000 }).toBe(true);
        expect(existsSync(join(dir, "updates"))).toBe(false);
      }
      process.emit("SIGTERM");
      ended = true;
      expect(await done).toBe(0);
    } finally {
      // A failed assertion must not leave the worker running (and
      // reconnecting) after the test: stop it and wait for run() to end.
      if (done && !ended) {
        process.emit("SIGTERM");
        await done;
      }
      await server.close();
    }
  });
}
