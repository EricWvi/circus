import { execFile } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";

const require = createRequire(import.meta.url);

/** The Claude Agent SDK's version (its package.json is not exported). */
export function sdkVersion(): string {
  const entry = require.resolve("@anthropic-ai/claude-agent-sdk");
  return (JSON.parse(readFileSync(join(dirname(entry), "package.json"), "utf8")) as { version: string }).version;
}

/** The version of the CLI bundled with the SDK, from `claude --version`; empty if unknown. */
export function bundledClaudeVersion(): Promise<string> {
  const names = [`@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}`,
    `@anthropic-ai/claude-agent-sdk-${process.platform}-${process.arch}-musl`];
  for (const name of names) {
    let bin: string;
    try {
      bin = join(dirname(require.resolve(`${name}/package.json`)), process.platform === "win32" ? "claude.exe" : "claude");
    } catch {
      continue;
    }
    return new Promise((done) => {
      execFile(bin, ["--version"], { timeout: 10_000, maxBuffer: 64 << 10 }, (error, stdout) => {
        done(error ? "" : String(stdout).trim().split(/\s+/)[0]?.slice(0, 64) ?? "");
      });
    });
  }
  return Promise.resolve("");
}
