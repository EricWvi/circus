import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";
import { loadConfig } from "./config.js";
import { WorkerLink } from "./link/index.js";
import { createLogger } from "./log.js";

function bootID(): string {
  const bytes = randomBytes(16);
  bytes.writeUIntBE(Date.now(), 0, 6);
  bytes[6] = (bytes[6] & 0x0f) | 0x70;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = bytes.toString("hex");
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join("-");
}

async function main(): Promise<void> {
  const config = loadConfig();
  const log = createLogger(config.logLevel);
  const { version } = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8")) as { version: string };
  const link = new WorkerLink({
    ...config, bootId: bootID(), clientVersion: version, log,
    attach: { maxSessions: config.maxSessions, sdkVersion: "", claudeVersion: "", localOnly: true },
  });
  link.onReady((outcomes) => log.info("worker attached", { resumed: outcomes.length }));
  const close = () => { void link.close(); };
  process.once("SIGINT", close);
  process.once("SIGTERM", close);
  link.start();
  try {
    for await (const { event } of link.hints()) {
      if (event.case !== undefined) {
        log.info("worker hint", { kind: event.case, session_id: event.value.sessionId });
      }
    }
    await link.closed;
  } finally {
    process.removeListener("SIGINT", close);
    process.removeListener("SIGTERM", close);
    await link.close();
  }
}

await main().catch((error: unknown) => {
  createLogger("error").error("worker stopped", { error: String(error).slice(0, 1024) });
  process.exitCode = 1;
});
