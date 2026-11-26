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

/**
 * How long an approval waits for its call's message. The message is already
 * queued when canUseTool runs, behind at most a few control-plane round trips
 * of the message loop: 5 s is ample, and bounds the wait if it never comes.
 */
export const PUBLISH_WAIT_MS = 5_000;

/** How a wait for a tool use's message ended. */
export type PublishWait = "published" | "timed_out" | "cancelled";

/**
 * The main thread's tool uses whose assistant message the message loop has
 * published, for approvals to wait on. The CLI writes the assistant message
 * carrying a tool_use before it asks canUseTool about that call, but the SDK
 * calls canUseTool as soon as it reads the request, while the message may
 * still wait in the SDK's queue for the message loop. The control plane
 * records APPROVAL_PENDING with the request: requested only once the call's
 * message is published, the card never precedes the call in the view.
 * Subagents' messages are not recorded: their calls do not wait, and their
 * traffic must not evict the main thread's calls.
 */
export class PublishedToolUses {
  #ids = new Set<string>();
  #waiting = new Map<string, Set<() => void>>();
  #ended = false;

  constructor(readonly capacity = 512, readonly boundMs = PUBLISH_WAIT_MS) {}

  /** Records the tool uses of a main-thread assistant message the message loop has published. */
  published(message: Record<string, unknown>): void {
    if (message.type !== "assistant" || message.parent_tool_use_id) return;
    for (const { id } of toolUsesOf(message)) {
      this.#ids.delete(id);
      this.#ids.add(id);
      if (this.#ids.size > this.capacity) this.#ids.delete(this.#ids.values().next().value!);
      for (const wake of this.#waiting.get(id) ?? []) wake();
      this.#waiting.delete(id);
    }
  }

  /**
   * Waits until the tool use is published, for at most boundMs; cancelled if
   * the signal aborts or the message loop ends first.
   */
  async wait(toolUseId: string, signal: AbortSignal): Promise<PublishWait> {
    if (this.#ids.has(toolUseId)) return "published";
    if (this.#ended || signal.aborted) return "cancelled";
    const { promise, resolve } = Promise.withResolvers<void>();
    const waiting = this.#waiting.get(toolUseId) ?? new Set();
    this.#waiting.set(toolUseId, waiting);
    const wake = () => resolve();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      resolve();
    }, this.boundMs);
    waiting.add(wake);
    signal.addEventListener("abort", wake, { once: true });
    try {
      await promise;
    } finally {
      clearTimeout(timer);
      signal.removeEventListener("abort", wake);
      waiting.delete(wake);
      if (!waiting.size && this.#waiting.get(toolUseId) === waiting) this.#waiting.delete(toolUseId);
    }
    if (this.#ids.has(toolUseId)) return "published";
    return timedOut ? "timed_out" : "cancelled";
  }

  /** The message loop ended: nothing more is published, and every waiter is cancelled. */
  end(): void {
    this.#ended = true;
    for (const waiting of this.#waiting.values()) for (const wake of waiting) wake();
    this.#waiting.clear();
  }
}

/** The tool whose calls become question approvals. */
export const QUESTION_TOOL = "AskUserQuestion";
/** Tools the worker removes: plan mode needs an approval flow it does not have. */
export const DISALLOWED_TOOLS = ["EnterPlanMode", "ExitPlanMode"];
/** The largest AskUserQuestion input the control plane stores. */
export const QUESTION_INPUT_BYTES = 32 * 1024;

/** A question approval's display: its first question, clipped; empty when there is none. */
export function questionDisplay(input: Record<string, unknown>): string {
  const questions = input.questions;
  const first = Array.isArray(questions) ? (questions[0] as { question?: unknown } | undefined)?.question : undefined;
  return typeof first === "string" ? clipUtf8(first, DISPLAY_BYTES) : "";
}

/** What canUseTool tells the model when it denies a call. */
export const denials = {
  subagentQuestion: "Only the main conversation can ask the user questions; AskUserQuestion is not available to subagents. " +
    "Continue without asking, and say in your result what you would have asked.",
  questionAsked: "Your questions were shown to the user. End your turn now, without guessing the answers: " +
    "the user's answers will arrive as the next user message.",
  questionTooLarge: `Your questions were not shown to the user: their input is larger than ${QUESTION_INPUT_BYTES / 1024} KiB. ` +
    "Ask again with shorter questions and options.",
  questionInvalid: (why: string) => `Your questions were not shown to the user: ${why.slice(0, 512)}. ` +
    "Ask again with a corrected AskUserQuestion call.",
  questionFailed: "Your questions could not be shown to the user. Continue without the answers, and tell the user what you wanted to ask.",
  toolSent: "This call was sent to the user for approval. Do not retry it until the user approves it: " +
    "if they do, you will be told to retry it.",
  toolFailed: "This call was not allowed, and asking the user for approval failed. Do not retry it; " +
    "continue with work that does not need it, and tell the user it was not run.",
  ending: "The session is ending.",
} as const;
