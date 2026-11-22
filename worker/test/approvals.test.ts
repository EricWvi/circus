import { createHash } from "node:crypto";
import { describe, expect, test } from "vitest";
import { approvalSubject, canonicalJson } from "../src/runner/approvals.js";

const sha = (text: string) => createHash("sha256").update(text, "utf8").digest("hex");
const key = (tool: string, input: unknown) => approvalSubject(tool, input, "/work/project")?.matchKey;

describe("approval match keys", () => {
  test("shell tools: the command verbatim", () => {
    expect(key("Bash", { command: "rm -rf x", description: "remove", timeout: 5 })).toBe(sha("rm -rf x"));
    expect(key("PowerShell", { command: "Remove-Item x" })).toBe(sha("Remove-Item x"));
    expect(key("Bash", { description: "no command" })).toBeUndefined();
  });

  test("file tools: the absolute path, relative paths against the session cwd", () => {
    expect(key("Write", { file_path: "/etc/hosts", content: "a" })).toBe(sha("/etc/hosts"));
    expect(key("Edit", { file_path: "src/a.ts", old_string: "a", new_string: "b" })).toBe(sha("/work/project/src/a.ts"));
    expect(key("MultiEdit", { file_path: "./b.ts", edits: [] })).toBe(sha("/work/project/b.ts"));
    expect(key("NotebookEdit", { notebook_path: "n.ipynb", new_source: "x" })).toBe(sha("/work/project/n.ipynb"));
  });

  test("tools keyed on stable fields ignore the free text the model rewrites", () => {
    expect(key("WebFetch", { url: "https://example.com", prompt: "summarize" })).toBe(sha("https://example.com"));
    expect(key("WebFetch", { url: "https://example.com", prompt: "other words" })).toBe(sha("https://example.com"));
    expect(key("WebSearch", { query: "circus", allowed_domains: ["a"] })).toBe(sha("circus"));
    expect(key("Glob", { pattern: "**/*.ts", path: "/src" })).toBe(sha(canonicalJson({ pattern: "**/*.ts", path: "/src" })));
    expect(key("Glob", { pattern: "**/*.ts" })).toBe(sha(canonicalJson({ pattern: "**/*.ts" })));
    expect(key("Grep", { pattern: "x", path: "/src", glob: "*.ts", output_mode: "content", "-n": true }))
      .toBe(sha(canonicalJson({ pattern: "x", path: "/src", glob: "*.ts" })));
    expect(key("Grep", { pattern: "x", type: "js", "-i": true })).toBe(sha(canonicalJson({ pattern: "x", type: "js" })));
    expect(key("Agent", { subagent_type: "Explore", prompt: "look", description: "d" })).toBe(sha("Explore"));
    expect(key("Task", { subagent_type: "Plan", prompt: "p" })).toBe(sha("Plan"));
  });

  test("MCP and other tools: canonical JSON of the whole input", () => {
    const input = { b: 1, a: { d: [1, { f: 2, e: 3 }], c: "x" } };
    expect(canonicalJson(input)).toBe('{"a":{"c":"x","d":[1,{"e":3,"f":2}]},"b":1}');
    expect(key("mcp__server__tool", input)).toBe(sha(canonicalJson(input)));
    // An MCP tool named like a built-in one is still keyed on its whole input.
    expect(key("mcp__web__WebFetch", { url: "u", prompt: "p" })).toBe(sha(canonicalJson({ prompt: "p", url: "u" })));
    expect(key("Read", { file_path: "/a", limit: 5 })).toBe(sha(canonicalJson({ file_path: "/a", limit: 5 })));
  });

  test("display names what is matched, cut at 1 KiB with the key", () => {
    expect(approvalSubject("Bash", { command: "echo hi" }, "/")?.display).toBe("echo hi");
    expect(approvalSubject("WebFetch", { url: "https://x" }, "/")?.display).toBe("WebFetch https://x");
    const long = approvalSubject("Bash", { command: "x".repeat(2000) }, "/")!;
    expect(Buffer.byteLength(long.display)).toBeLessThanOrEqual(1024);
    expect(long.display.endsWith(`[${long.matchKey}]`)).toBe(true);
  });
});

test("Glob and Grep paths are keyed absolute", () => {
  expect(key("Glob", { pattern: "*.ts", path: "src" })).toBe(key("Glob", { pattern: "*.ts", path: "/work/project/src" }));
  expect(key("Grep", { pattern: "x", path: "./lib" })).toBe(sha(canonicalJson({ pattern: "x", path: "/work/project/lib" })));
});
