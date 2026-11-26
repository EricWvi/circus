import { createHash } from "node:crypto";
import { afterEach, describe, expect, test, vi } from "vitest";
import { PUBLISH_WAIT_MS, PublishedToolUses, approvalReason, approvalSubject, canonicalJson } from "../src/runner/approvals.js";

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

test("a denial's reason is clipped to 1 KiB, and empty when the permission mode gave none", () => {
  expect(approvalReason(undefined)).toBe("");
  expect(approvalReason("")).toBe("");
  expect(approvalReason("fake classifier: destructive command")).toBe("fake classifier: destructive command");
  const long = approvalReason("é".repeat(1000));
  expect(Buffer.byteLength(long)).toBeLessThanOrEqual(1024);
  expect(long).toBe("é".repeat(512));
});

describe("published tool uses", () => {
  const assistant = (ids: string[], parent: string | null = null) => ({
    type: "assistant", parent_tool_use_id: parent, message: { content: ids.map((id) => ({ type: "tool_use", id, name: "Bash", input: {} })) },
  });
  const never = new AbortController().signal;

  afterEach(() => {
    vi.useRealTimers();
  });

  test("a wait ends when an assistant message carrying the tool use is published", async () => {
    const published = new PublishedToolUses();
    const waiting = published.wait("toolu_b", never);
    published.published({ type: "user", message: { content: [{ type: "tool_result", tool_use_id: "toolu_b" }] } });
    published.published(assistant(["toolu_a", "toolu_b"]));
    expect(await waiting).toBe("published");
    expect(await published.wait("toolu_a", never)).toBe("published");
  });

  test("a wait is cancelled when its signal aborts or the message loop ends", async () => {
    const published = new PublishedToolUses();
    const abort = new AbortController();
    const aborted = published.wait("toolu_a", abort.signal);
    const ended = published.wait("toolu_b", never);
    abort.abort();
    expect(await aborted).toBe("cancelled");
    published.end();
    expect(await ended).toBe("cancelled");
    expect(await published.wait("toolu_c", never)).toBe("cancelled");
  });

  test("a wait times out after its bound", async () => {
    vi.useFakeTimers();
    const published = new PublishedToolUses(512, 5_000);
    let outcome: string | undefined;
    void published.wait("toolu_a", never).then((o) => { outcome = o; });
    await vi.advanceTimersByTimeAsync(4_999);
    expect(outcome).toBeUndefined();
    await vi.advanceTimersByTimeAsync(1);
    expect(outcome).toBe("timed_out");
    expect(PUBLISH_WAIT_MS).toBe(5_000);
  });

  test("only the latest tool uses are kept", async () => {
    const published = new PublishedToolUses(2);
    published.published(assistant(["toolu_1", "toolu_2", "toolu_3"]));
    published.end();
    expect(await published.wait("toolu_1", never)).toBe("cancelled");
    expect(await published.wait("toolu_3", never)).toBe("published");
  });

  test("subagents' tool uses do not evict a main-thread call still to be asked about", async () => {
    // The main thread starts a subagent and runs Bash in one message; the
    // subagent publishes more than the capacity before the CLI asks about Bash.
    vi.useFakeTimers();
    const published = new PublishedToolUses(512, 5_000);
    published.published(assistant(["toolu_agent", "toolu_bash"]));
    for (let i = 0; i < 520; i++) published.published(assistant([`toolu_sub_${i}`], "toolu_agent"));
    const waiting = published.wait("toolu_bash", never);
    await vi.advanceTimersByTimeAsync(5_000);
    expect(await waiting).toBe("published");
  });
});
