import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";

const require = createRequire(import.meta.url);

/** The Claude Agent SDK's version (its package.json is not exported). */
export function sdkVersion(): string {
  const entry = require.resolve("@anthropic-ai/claude-agent-sdk");
  return (JSON.parse(readFileSync(join(dirname(entry), "package.json"), "utf8")) as { version: string }).version;
}
