import { createHash } from "node:crypto";
import { isAbsolute, resolve } from "node:path";

const DISPLAY_BYTES = 1024;
const REASON_BYTES = 1024;
const TOOL_BYTES = 256;

export interface ApprovalSubject {
  tool: string;
  /** SHA-256 of the matched text, 64 lowercase hex digits. */
  matchKey: string;
  display: string;
}

export const sha256 = (text: string) => createHash("sha256").update(text, "utf8").digest("hex");

/** Truncates text to at most limit UTF-8 bytes at a character boundary. */
export function clipUtf8(text: string, limit: number): string {
  if (Buffer.byteLength(text) <= limit) return text;
  const bytes = Buffer.from(text, "utf8");
  let end = limit;
  while (end > 0 && (bytes[end] & 0xc0) === 0x80) end--;
  return bytes.subarray(0, end).toString("utf8");
}

/** The fake worker's display rule: long text is cut and names its key. */
function display(text: string, matchKey: string): string {
  if (Buffer.byteLength(text) <= DISPLAY_BYTES) return text;
  const suffix = ` … [${matchKey}]`;
  return clipUtf8(text, DISPLAY_BYTES - Buffer.byteLength(suffix)) + suffix;
}

/** JSON with object keys sorted at every level, so equal inputs give equal text. */
export function canonicalJson(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (value && typeof value === "object") {
    const entries = Object.entries(value as Record<string, unknown>)
      .filter(([, v]) => v !== undefined)
      .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
    return `{${entries.map(([k, v]) => `${JSON.stringify(k)}:${canonicalJson(v)}`).join(",")}}`;
  }
  return JSON.stringify(value) ?? "null";
}

const shellTools = new Set(["Bash", "PowerShell"]);
const fileTools: Record<string, string> = { Write: "file_path", Edit: "file_path", MultiEdit: "file_path", NotebookEdit: "notebook_path" };

/** Tools keyed on some of their input fields: the stable ones, not free text the model rewrites. */
const keyFields: Record<string, string[]> = {
  WebFetch: ["url"],
  WebSearch: ["query"],
  Glob: ["pattern", "path"],
  Grep: ["pattern", "path", "glob", "type"],
  Agent: ["subagent_type"],
  Task: ["subagent_type"],
};

/**
 * What an approval of one tool use matches, by tool:
 * - Bash, PowerShell: the command, verbatim;
 * - Write, Edit, MultiEdit: file_path, and NotebookEdit: notebook_path,
 *   made absolute against the session cwd;
 * - WebFetch: url; WebSearch: query; Glob: pattern and path; Grep: pattern,
 *   path, glob and type (path made absolute against the cwd); Agent (Task):
 *   subagent_type. A single field is matched verbatim, several as canonical
 *   JSON of those present;
 * - MCP tools (mcp__*) and every other tool: the whole tool input as
 *   canonical JSON (keys sorted at every level), so only an identical
 *   input matches.
 * The match key is the SHA-256 of that text; the tool name is the SDK's.
 */
export function approvalSubject(tool: string, input: unknown, cwd: string): ApprovalSubject | undefined {
  if (Buffer.byteLength(tool) > TOOL_BYTES || !tool) return undefined;
  let fields = input && typeof input === "object" ? input as Record<string, unknown> : {};
  let text: string;
  let shown: string;
  if (shellTools.has(tool)) {
    if (typeof fields.command !== "string") return undefined;
    text = shown = fields.command;
  } else if (fileTools[tool]) {
    const path = fields[fileTools[tool]];
    if (typeof path !== "string" || !path) return undefined;
    text = shown = isAbsolute(path) ? path : resolve(cwd, path);
  } else if (keyFields[tool] && !tool.startsWith("mcp__")) {
    const names = keyFields[tool];
    // Glob and Grep search a path; the same place gives the same key, however written.
    if (typeof fields.path === "string" && fields.path && names.includes("path") && !isAbsolute(fields.path)) {
      fields = { ...fields, path: resolve(cwd, fields.path) };
    }
    if (names.length === 1) {
      text = String(fields[names[0]] ?? "");
    } else {
      text = canonicalJson(Object.fromEntries(names.filter((name) => fields[name] !== undefined).map((name) => [name, fields[name]])));
    }
    shown = `${tool} ${text}`;
  } else {
    text = canonicalJson(input ?? {});
    shown = `${tool} ${text}`;
  }
  const matchKey = sha256(text);
  return { tool, matchKey, display: display(shown, matchKey) };
}

/** Why the permission mode denied a call, clipped; empty when it gave no reason (clients word that themselves). */
export const approvalReason = (reason: string | undefined) => clipUtf8(reason ?? "", REASON_BYTES);

export interface ToolUse {
  tool: string;
  input: unknown;
  agentId: string;
}

/** Remembers the latest tool uses by id, so a denial can be matched to its input. */
export class ToolUses {
  #uses = new Map<string, ToolUse>();

  constructor(readonly capacity = 512) {}

  remember(toolUseId: string, use: ToolUse): void {
    if (!toolUseId) return;
    this.#uses.delete(toolUseId);
    this.#uses.set(toolUseId, use);
    if (this.#uses.size > this.capacity) this.#uses.delete(this.#uses.keys().next().value!);
  }

  get(toolUseId: string): ToolUse | undefined {
    return this.#uses.get(toolUseId);
  }
}

/** Collects tool_use blocks of an SDK assistant message. */
export function toolUsesOf(message: Record<string, unknown>): Array<{ id: string; name: string; input: unknown }> {
  const content = (message.message as { content?: unknown } | undefined)?.content;
  if (!Array.isArray(content)) return [];
  return content.flatMap((block) => {
    const b = block as { type?: unknown; id?: unknown; name?: unknown; input?: unknown };
    return b.type === "tool_use" && typeof b.id === "string" && typeof b.name === "string"
      ? [{ id: b.id, name: b.name, input: b.input }] : [];
  });
}
