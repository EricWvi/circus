// The mock's sessions: in memory, for this tab only. A message gets a short
// scripted reply, emitted as the raw events the control plane streams (the
// Claude Agent SDK message shapes the Go fake worker publishes), so the
// pages and the transcript reducer run the same code as against a server:
// streamed text, one Bash call, and an approval when the message asks for
// one ("approve" anywhere in it).

import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import type {
  QueueReason,
  RawSessionEvent,
  SessionApproval,
  SessionDetails,
  SessionEventKind,
  SessionService,
  SessionSummary,
} from "@/api/types"
import { sessionPermissionMode } from "@/lib/permission-mode"
import { viewState, type ServerSessionState, type TurnOutcome } from "@/lib/session-state"
import { titleFromMessage } from "@/lib/title"
import type { SeedSession } from "@/mock/data"

import { ledgerStatusOf, type MockSessionLedger } from "./session-ledger"
import { workspaceStatus, type MockWorkspace } from "./workspaces"

export interface MockApproval {
  approvalId: string
  toolUseId: string
  command: string
  state: "pending" | "approved" | "denied"
  createdAtUnixMs: number
}

export interface MockSession {
  sessionId: string
  projectId: string
  title: string
  device: string
  cwd: string
  createdAtUnixMs: number
  lastActivityAtUnixMs: number
  state: ServerSessionState
  lastTurnOutcome: TurnOutcome
  /** Durable events, in seq order. */
  events: RawSessionEvent[]
  approvals: MockApproval[]
  /** Inputs not yet taken by a turn. */
  queue: { inputId: string; text: string; approval: boolean }[]
  turns: number
  /** The request that created the session, for retries of it; forgotten with the session. */
  created?: { clientRequestId: string; locationIndex: number; providerId?: string; permissionMode: string }
  /** Created in a default location: its folder, which holds turns until ready. */
  workspace?: MockWorkspace
  /** The model provider's id; empty or missing for local login. */
  providerId?: string
  /** The permission mode the session was created with; empty or missing for the default. */
  permissionMode?: string
  /** Why the session waits; missing or `unspecified` when it doesn't. */
  queueReason?: QueueReason
  /**
   * When the connection of the worker holding the lease closed; missing or
   * 0 while it is connected (or no lease is held).
   */
  workerDisconnectedAtUnixMs?: number
  /**
   * Taken by a worker: the mock keeps the lease from the first turn until
   * the session stops or changes provider, as a worker keeping its CLI.
   */
  leased?: boolean
  /** The session's ledger snapshot; missing for a session without one. */
  ledger?: MockSessionLedger
}

/** How the scripted replies pace themselves; tests shorten it. */
export interface MockSessionTiming {
  /** Between the steps of a turn. */
  stepMs: number
  /** Between the chunks of streamed text. */
  chunkMs: number
}

export const defaultSessionTiming: MockSessionTiming = { stepMs: 400, chunkMs: 120 }

const APPROVAL_REASON = "Pushing to a remote needs your approval"
const DENIAL = `Permission for this action was denied by the Claude Code auto mode classifier. Reason: ${APPROVAL_REASON}.`
const INTERRUPTED_TOOL =
  "The user doesn't want to proceed with this tool use. The tool use was rejected (eg. if it was a file edit, the new_string was NOT written to the file). " +
  "STOP what you are doing and wait for the user to tell you how to proceed. Correction to the text above: this call was stopped while running, not refused before it ran. " +
  "It may have run in part or in full, and its changes may have been written. No permission prompt was involved. " +
  "Work it started elsewhere (a remote command, a background process) may still be running, so check before starting the same work again. " +
  "The instruction to stop and wait still stands."

// --- SDK message shapes -------------------------------------------------
// As the real worker publishes them (Claude Code's stream-json messages).
// Usage, costs and timings are placeholders.

const envelope = (type: string, uuid: string) => ({ type, uuid, session_id: "mock", parent_tool_use_id: null })
/** A user message; `note` names the kind of one the worker wrote for the model (docs/wire.md). */
const userMessage = (uuid: string, content: unknown, note?: "approval_retry" | "user_interrupt") => ({
  ...envelope("user", uuid),
  message: { role: "user", content },
  ...(note ? { isSynthetic: true, circus_note: note } : {}),
})
const usage = { input_tokens: 0, output_tokens: 0, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 }
/** The user message that started a turn, named by its first response and first block. */
const namingInput = (userMessageId?: string) =>
  userMessageId ? { user_message_uuid: userMessageId, user_message_uuids: [userMessageId] } : {}
const assistantMessage = (messageId: string, block: object, aborted = false, userMessageId?: string) => ({
  ...envelope("assistant", nanoid()),
  message: {
    id: messageId,
    type: "message",
    role: "assistant",
    model: "claude-mock",
    content: [block],
    stop_reason: null,
    stop_sequence: null,
    context_management: null,
    usage,
  },
  ...namingInput(userMessageId),
  ...(aborted ? { aborted: true } : {}),
})
const streamEvent = (messageId: string, event: object, extra: object = {}) => ({
  ...envelope("stream_event", nanoid()),
  api_message_id: messageId,
  event,
  ...extra,
})
const messageStart = (messageId: string, userMessageId?: string) =>
  streamEvent(
    messageId,
    {
      type: "message_start",
      message: { id: messageId, type: "message", role: "assistant", model: "claude-mock", content: [], stop_reason: null, stop_sequence: null, usage },
    },
    { thinking_display: "updates", ttft_ms: 0, ...(userMessageId ? { owed_rows: [], ...namingInput(userMessageId) } : {}) },
  )
const messageDelta = (messageId: string, stopReason: "end_turn" | "tool_use" | null) =>
  streamEvent(
    messageId,
    stopReason === null
      ? {
          type: "message_delta",
          context_management: null,
          delta: { stop_reason: null, stop_sequence: null, stop_details: null, container: null },
          usage: { ...usage, fallback_credit: null, iterations: [], output_tokens_details: { thinking_tokens: 0 }, server_tool_use: { web_fetch_requests: 0, web_search_requests: 0 } },
        }
      : {
          type: "message_delta",
          delta: { stop_reason: stopReason, stop_sequence: null, safeguard_results: [{ type: "dangerous_tool_use", status: { type: "available", tool_uses: {} } }] },
          usage: { output_tokens: 0 },
        },
  )
const textBlock = (text: string) => ({ type: "text", text })
const toolUse = (id: string, command: string) => ({ type: "tool_use", id, name: "Bash", input: { command } })
/** A tool's result: its text, Claude Code's own record of it and how the call was let run (or why not). */
const toolResult = (uuid: string, id: string, content: string, isError: boolean, useResult: unknown, meta: object) => ({
  ...userMessage(uuid, [{ type: "tool_result", tool_use_id: id, content, is_error: isError }]),
  tool_use_result: useResult,
  tool_result_meta: [{ id, ...meta }],
})
const bashResult = (id: string, output: string, approved: boolean) =>
  toolResult(nanoid(), id, output, false, { stdout: output, stderr: "", interrupted: false, isImage: false, noOutputExpected: false }, {
    permission_decision: approved
      ? { decision: "accept", reason_type: "hook", source: "hook" }
      : { decision: "accept", reason_type: "classifier", source: "config" },
  })
const denialResult = (id: string) =>
  toolResult(nanoid(), id, DENIAL, true, `Error: ${DENIAL}`, {
    non_execution_kind: "automode-blocked",
    permission_decision: { decision: "reject", reason_type: "classifier", source: "config" },
  })
const interruptedResult = (id: string) =>
  toolResult(nanoid(), id, INTERRUPTED_TOOL, true, "User rejected tool use", { non_execution_kind: "user-rejected" })
const permissionDenied = (id: string) => ({
  type: "system",
  subtype: "permission_denied",
  uuid: nanoid(),
  session_id: "mock",
  tool_name: "Bash",
  tool_use_id: id,
  decision_reason_type: "classifier",
  decision_reason: APPROVAL_REASON,
  message: DENIAL,
})
/** The placeholder subagent statistics of a result. */
const subagentStats = {
  by_type: {},
  completed: 0,
  failed: 0,
  killed: { parent: 0, system: 0, user: 0 },
  max_depth: 0,
  refused: { budget: 0, concurrency_limit: 0, depth_limit: 0 },
  requested: { background: 0, foreground: 0, unset: 0 },
  spawned: 0,
  spawned_by_subagents: 0,
  started_in_background: 0,
}
/** What any result says of its turn; placeholders. */
const resultCommon = {
  usage: { ...usage, cache_creation: { ephemeral_1h_input_tokens: 0, ephemeral_5m_input_tokens: 0 } },
  modelUsage: {},
  model_usage_since_last_result: {},
  total_cost_usd: 0,
  duration_ms: 0,
  duration_api_ms: 0,
  num_turns: 1,
  permission_denials: [],
  begun_message_ids: [],
  result_index: 0,
  fast_mode_state: "off",
  fast_mode_disabled_reason: "sdk_opt_in_required",
  queued_turn_count: 0,
  safety_stops: 0,
  subagent_stats: subagentStats,
}
/** What a successful result adds: the first request's figures; placeholders. */
const firstRequest = {
  api_error_status: null,
  first_request_input_tokens: 0,
  first_request_cache_read_input_tokens: 0,
  first_content_frame_ms: 0,
  request_sent_wall_ms: 0,
  time_to_request_ms: 0,
  ttft_ms: 0,
  ttft_stream_ms: 0,
}
/**
 * A turn's result, with the key set of the fake worker's: `aborted` says
 * what the user interrupted, `userMessageId` is the input that started the turn.
 */
const result = (
  text: string,
  userMessageId: string | undefined,
  failure?: string,
  aborted?: "aborted_streaming" | "aborted_tools",
) => ({
  type: "result",
  uuid: nanoid(),
  session_id: "mock",
  ...resultCommon,
  ...namingInput(userMessageId),
  subtype: failure ? "error_during_execution" : "success",
  is_error: failure !== undefined,
  stop_reason: aborted === "aborted_tools" ? "tool_use" : failure ? null : "end_turn",
  ...(failure ? { errors: [failure] } : { ...firstRequest, result: text, terminal_reason: "completed" }),
  ...(aborted ? { terminal_reason: aborted } : {}),
})
/**
 * Claude Code's synthetic reply when the API rejects the request (here a
 * rejected key): an assistant message of its own, and a result that is an
 * error while its subtype says success.
 */
const AUTH_FAILURE =
  'Failed to authenticate. API Error: 401 {"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}'
const syntheticMessage = (text: string, userMessageId?: string) => {
  const message = assistantMessage(`msg_${nanoid()}`, textBlock(text), false, userMessageId)
  return { ...message, message: { ...message.message, model: "<synthetic>" }, error: "authentication_failed" }
}
const failedResult = (text: string, userMessageId: string | undefined) => ({
  ...result(text, userMessageId),
  is_error: true,
  api_error_status: 401,
  terminal_reason: "api_error",
})
const abortFailure = (reason: "aborted_streaming" | "aborted_tools") =>
  `[ede_diagnostic] result_type=user last_content_type=n/a stop_reason=${reason === "aborted_tools" ? "tool_use" : "null"}`

/** Appends events to a session as the control plane records them. */
export class Recorder {
  constructor(
    private readonly session: MockSession,
    private readonly now: () => number,
    private readonly emit: (event: RawSessionEvent) => void = () => {},
  ) {}

  /** `tsUnixMs`: the event's time when the change has one of its own (a workspace's `updatedAt`). */
  durable(kind: SessionEventKind, payload: object | null, tsUnixMs = this.now()) {
    const s = this.session
    // Claude Code stamps the user and assistant messages it records.
    const type = (payload as { type?: unknown } | null)?.type
    if (type === "user" || type === "assistant") payload = { ...payload, timestamp: new Date(tsUnixMs).toISOString() }
    const event: RawSessionEvent = {
      seq: (s.events.at(-1)?.seq ?? 0) + 1,
      tsUnixMs,
      kind,
      payloadJson: payload === null ? "" : JSON.stringify(payload),
      ephemeral: false,
      truncated: false,
    }
    s.events.push(event)
    s.lastActivityAtUnixMs = Math.max(s.lastActivityAtUnixMs, event.tsUnixMs)
    this.emit(event)
  }

  ephemeral(kind: SessionEventKind, payload: object, tsUnixMs = this.now()) {
    this.emit({ seq: 0, tsUnixMs, kind, payloadJson: JSON.stringify(payload), ephemeral: true, truncated: false })
  }

  /** A new input; returns its id. */
  input(text: string, clientMsgId: string = nanoid()): string {
    const inputId = nanoid()
    this.durable("input_accepted", { input_id: inputId, client_msg_id: clientMsgId, priority: "normal", text })
    if (this.session.title === "") this.session.title = titleFromMessage(text) ?? ""
    return inputId
  }

  turnStarted() {
    this.session.turns++
    this.session.state = "running"
    this.durable("turn_started", { turn_no: String(this.session.turns), origin: "input" })
  }

  text(text: string, messageId = `msg_${nanoid()}`, aborted = false, userMessageId?: string) {
    this.durable("assistant", assistantMessage(messageId, textBlock(text), aborted, userMessageId))
  }

  /** A whole recorded response: a text block, or a Bash call. */
  response(block: { text: string } | { toolId: string; command: string }, userMessageId?: string) {
    const messageId = `msg_${nanoid()}`
    if ("text" in block) this.text(block.text, messageId, false, userMessageId)
    else this.durable("assistant", assistantMessage(messageId, toolUse(block.toolId, block.command), false, userMessageId))
  }
}

// --- Seeds --------------------------------------------------------------

const MINUTE = 60_000

/** A seeded session with a short history that ends as `seed.ending` says. */
export function seedSession(seed: SeedSession, cwd: string, loadedAt: number): MockSession {
  const at = loadedAt - seed.minutesAgo * MINUTE
  const session: MockSession = {
    sessionId: seed.id,
    projectId: seed.projectId,
    title: "",
    device: seed.device,
    cwd,
    createdAtUnixMs: at - 45 * MINUTE,
    lastActivityAtUnixMs: at - 45 * MINUTE,
    state: "idle",
    lastTurnOutcome: "unspecified",
    events: [],
    approvals: [],
    queue: [],
    turns: 0,
    providerId: seed.provider ?? "",
    ...(seed.leased ? { leased: true } : {}),
    ...(seed.workerDisconnectedSecondsAgo !== undefined
      ? { workerDisconnectedAtUnixMs: loadedAt - seed.workerDisconnectedSecondsAgo * 1_000 }
      : {}),
  }
  let clock = at - 40 * MINUTE
  const r = new Recorder(session, () => (clock += 1_000))
  const exchange = (text: string, reply: string) => {
    const inputId = r.input(text)
    r.turnStarted()
    r.durable("user", userMessage(inputId, text))
    r.response({ text: reply }, inputId)
    r.durable("result", result(reply, inputId))
    session.state = "idle"
    session.lastTurnOutcome = "completed"
  }
  exchange(seed.title, `Sure. I'll look into "${seed.title}" on ${seed.device} and report back.`)
  clock = at - 2 * MINUTE
  switch (seed.ending) {
    case "idle":
      break
    case "approval": {
      const inputId = r.input(seed.followUp ?? "Go on")
      r.turnStarted()
      r.durable("user", userMessage(inputId, seed.followUp ?? "Go on"))
      const toolId = `toolu_${nanoid()}`
      const command = seed.command ?? "git push"
      r.response({ toolId, command }, inputId)
      r.durable("permission_denied", permissionDenied(toolId))
      const approvalId = nanoid()
      session.approvals.push({ approvalId, toolUseId: toolId, command, state: "pending", createdAtUnixMs: clock })
      r.durable("approval_pending", { approval_id: approvalId, tool: "Bash", device: seed.device, tool_use_id: toolId, display: command, reason: APPROVAL_REASON })
      r.durable("user", denialResult(toolId))
      r.text("I need your approval to push the branch.")
      r.durable("result", result("I need your approval to push the branch.", inputId))
      session.state = "idle"
      break
    }
    case "interrupted": {
      const inputId = r.input(seed.followUp ?? "Go on")
      r.turnStarted()
      r.durable("user", userMessage(inputId, seed.followUp ?? "Go on"))
      r.durable("turn_interrupted", { turn_no: String(session.turns), outcome: "interrupted" })
      session.state = "idle"
      session.lastTurnOutcome = "interrupted"
      break
    }
    case "stopped_tool": {
      // As abortTurn records a stop while a tool runs.
      const inputId = r.input(seed.followUp ?? "Go on")
      r.turnStarted()
      r.durable("user", userMessage(inputId, seed.followUp ?? "Go on"))
      const toolId = `toolu_${nanoid()}`
      r.response({ toolId, command: seed.command ?? "sleep 20" }, inputId)
      r.durable("user", interruptedResult(toolId))
      r.durable("user", userMessage(nanoid(), [textBlock("[Request interrupted by user for tool use]")], "user_interrupt"))
      r.durable("result", result("", inputId, abortFailure("aborted_tools"), "aborted_tools"))
      r.durable("turn_stopped", { turn_no: String(session.turns), outcome: "stopped" })
      session.state = "idle"
      session.lastTurnOutcome = "stopped"
      break
    }
    case "broken": {
      const inputId = r.input(seed.followUp ?? "Go on")
      r.turnStarted()
      r.durable("user", userMessage(inputId, seed.followUp ?? "Go on"))
      r.durable("error", { message: "transcript mirror failure" })
      r.durable("result", result("", inputId, "transcript mirror failure"))
      session.state = "broken"
      session.lastTurnOutcome = "error"
      break
    }
    case "stopped":
      r.durable("session_state", { state: "stopped", reason: "user" })
      session.state = "stopped"
      break
    case "queued": {
      const text = seed.followUp ?? "Go on"
      session.queue.push({ inputId: r.input(text), text, approval: false })
      session.state = "queued"
      break
    }
    case "running": {
      const text = seed.followUp ?? "Go on"
      const inputId = r.input(text)
      r.turnStarted()
      r.durable("user", userMessage(inputId, text))
      r.response({ text: "Starting the suite; it takes a few minutes." }, inputId)
      break
    }
  }
  session.lastActivityAtUnixMs = at
  return session
}

// --- The service --------------------------------------------------------

export function summaryOf(s: MockSession): SessionSummary {
  const pendingApprovalCount = s.approvals.filter((a) => a.state === "pending").length
  return {
    sessionId: s.sessionId,
    projectId: s.projectId,
    title: s.title,
    serverState: s.state,
    turnCount: s.turns,
    viewTruncated: false,
    state: viewState({ state: s.state, lastTurnOutcome: s.lastTurnOutcome, pendingApprovalCount }),
    lastActivityAtUnixMs: s.lastActivityAtUnixMs,
    pendingApprovalCount,
    createdAtUnixMs: s.createdAtUnixMs,
    ...(s.workspace ? { workspace: workspaceStatus(s.workspace) } : {}),
    providerId: s.providerId ?? "",
    queueReason: s.state === "queued" ? (s.queueReason ?? "unspecified") : "unspecified",
    workerDisconnectedAtUnixMs: s.workerDisconnectedAtUnixMs ?? 0,
    ...(s.ledger ? { ledger: ledgerStatusOf(s.ledger) } : {}),
  }
}

export const detailsOf = (s: MockSession): SessionDetails => ({
  ...summaryOf(s),
  device: s.device,
  cwd: s.cwd,
  permissionMode: sessionPermissionMode(s.permissionMode ?? ""),
})

/** What the session service needs from the rest of the mock backend. */
export interface MockSessionHost {
  sessions(): MockSession[]
  /** The session if it belongs to a project of the signed-in user; throws otherwise. */
  own(sessionId: string): MockSession
  /** Runs a handler after the simulated network delay, like the other mock calls. */
  delay(): Promise<void>
  now(): number
  timing: MockSessionTiming
  /**
   * Whether a worker may take the session for its next turn now; leases it
   * if so. When not, the host has recorded why (`queueReason`).
   */
  admit?(s: MockSession): boolean
  /** A turn completed; `input` is the message it answered. */
  turnEnded?(s: MockSession, input: string): void
  /**
   * Changes the session's provider ("" for local login) once the session
   * runs no turn and isn't stopped; throws as the server refuses.
   */
  setProvider?(s: MockSession, providerId: string): void
  /** The session stopped: its lease, if any, is released. */
  stopped?(s: MockSession): void
}

type Live = (event: RawSessionEvent) => void

/** What the rest of the mock backend can do to sessions. */
export interface MockSessionRuntime {
  /** The session RPCs besides listing, creating and retrying a workspace, which need the projects. */
  service: Omit<SessionService, "listSessions" | "createSession" | "retryWorkspace" | "deleteSession">
  /** Records an event as the control plane does: durable, or live only, at `tsUnixMs`. */
  record(s: MockSession, kind: SessionEventKind, payload: object, durable: boolean, tsUnixMs: number): void
  /** Starts the next turn if one waits and the session can run it. */
  wake(s: MockSession): void
  /** The session's lease ended without its holder: an open turn ends as interrupted. */
  leaseLost(s: MockSession): void
}

export function createMockSessionRuntime(host: MockSessionHost): MockSessionRuntime {
  const watchers = new Map<string, Set<Live>>()
  /** Pending steps of each session's running turn. */
  const turns = new Map<string, Turn>()

  interface Turn {
    timers: ReturnType<typeof setTimeout>[]
    /** Text streamed so far in the current block, with its message id and the input its message names. */
    streaming: { messageId: string; text: string; startedBy?: string } | null
    /** A Bash call waiting for its result. */
    tool: string | null
    /** The input that started the turn, until its first response names it. */
    startedBy?: string
    /** The input that started the turn. */
    inputId?: string
  }

  const recorder = (s: MockSession) =>
    new Recorder(s, host.now, (event) => {
      for (const live of watchers.get(s.sessionId) ?? []) live(event)
    })

  /** Runs the steps one after another, `stepMs` apart (a step may ask for longer). */
  function schedule(s: MockSession, steps: (() => number | void)[], startedBy?: string) {
    const turn: Turn = { timers: [], streaming: null, tool: null, startedBy, inputId: startedBy }
    turns.set(s.sessionId, turn)
    let i = 0
    const next = (wait: number) => {
      turn.timers = [
        setTimeout(() => {
          if (turns.get(s.sessionId) !== turn) return
          const step = steps[i++]
          if (!step) {
            turns.delete(s.sessionId)
            startNext(s)
            return
          }
          next(step() ?? host.timing.stepMs)
        }, wait),
      ]
    }
    next(host.timing.stepMs)
    return turn
  }

  /** Starts a response's stream; the turn's first one names the input that started it. */
  function begin(r: Recorder, turn: Turn, messageId: string) {
    const startedBy = turn.startedBy
    turn.startedBy = undefined
    r.ephemeral("assistant", messageStart(messageId, startedBy))
    return startedBy
  }

  /**
   * Streamed text: live chunks, then the durable block (before the block's
   * content_block_stop, as Claude Code publishes it).
   */
  function streamSteps(s: MockSession, turn: () => Turn | undefined, text: string): (() => number | void)[] {
    const r = recorder(s)
    const messageId = `msg_${nanoid()}`
    const words = text.split(/(?<=\s)/)
    const chunks: string[] = []
    for (let i = 0; i < words.length; i += 3) chunks.push(words.slice(i, i + 3).join(""))
    let startedBy: string | undefined
    return [
      () => {
        startedBy = begin(r, turn()!, messageId)
        r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_start", index: 0, content_block: textBlock("") }))
        turn()!.streaming = { messageId, text: "", startedBy }
        return host.timing.chunkMs
      },
      ...chunks.map((chunk) => () => {
        r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: chunk } }))
        turn()!.streaming!.text += chunk
        return host.timing.chunkMs
      }),
      () => {
        r.text(text, messageId, false, startedBy)
        r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_stop", index: 0 }))
        r.ephemeral("assistant", messageDelta(messageId, "end_turn"))
        r.ephemeral("assistant", streamEvent(messageId, { type: "message_stop" }))
        turn()!.streaming = null
      },
    ]
  }

  /** A streamed Bash tool use, whose durable block comes before its content_block_stop. */
  function streamToolUse(r: Recorder, turn: Turn, toolId: string, command: string) {
    const messageId = `msg_${nanoid()}`
    const startedBy = begin(r, turn, messageId)
    const input = JSON.stringify({ command })
    const half = Math.floor(input.length / 2)
    r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_start", index: 0, content_block: { type: "tool_use", id: toolId, name: "Bash", input: {} } }))
    for (const partial of [input.slice(0, half), input.slice(half)]) {
      r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_delta", index: 0, delta: { type: "input_json_delta", partial_json: partial } }))
    }
    r.durable("assistant", assistantMessage(messageId, toolUse(toolId, command), false, startedBy))
    r.ephemeral("assistant", streamEvent(messageId, { type: "content_block_stop", index: 0 }))
    r.ephemeral("assistant", messageDelta(messageId, "tool_use"))
    r.ephemeral("assistant", streamEvent(messageId, { type: "message_stop" }))
  }

  function toolSteps(s: MockSession, turn: () => Turn | undefined, command: string, output: string, approved: boolean) {
    const r = recorder(s)
    const toolId = `toolu_${nanoid()}`
    return [
      () => {
        streamToolUse(r, turn()!, toolId, command)
        turn()!.tool = toolId
        return host.timing.stepMs * 2
      },
      () => {
        r.durable("user", bashResult(toolId, output, approved))
        turn()!.tool = null
      },
    ]
  }

  /** Starts a turn for the next queued input, if the session can run one (and its folder is ready). */
  function startNext(s: MockSession) {
    if (turns.has(s.sessionId) || s.state === "stopped" || s.state === "broken") return
    if (s.workspace && s.workspace.state !== "ready") return
    if (s.queue.length > 0 && host.admit && !host.admit(s)) {
      s.state = "queued"
      return
    }
    const input = s.queue.shift()
    if (!input) {
      if (s.state === "running" || s.state === "queued") s.state = "idle"
      return
    }
    const r = recorder(s)
    let turn: Turn | undefined
    const current = () => turn
    const finish = (text: string) => () => {
      r.durable("result", result(text, input.inputId))
      s.state = "idle"
      s.lastTurnOutcome = "completed"
      host.turnEnded?.(s, input.text)
    }
    const start = () => {
      r.turnStarted()
      r.durable("user", userMessage(input.inputId, input.text, input.approval ? "approval_retry" : undefined))
    }
    let steps: (() => number | void)[]
    if (input.approval) {
      const command = input.text
      steps = [
        start,
        ...toolSteps(s, current, command, "branch 'fix' set up to track 'origin/fix'.", true),
        ...streamSteps(s, current, "Pushed. The branch is on the remote now."),
        finish("Pushed."),
      ]
    } else if (input.text.includes("401")) {
      steps = [
        start,
        () => {
          r.durable("assistant", syntheticMessage(AUTH_FAILURE, input.inputId))
          r.durable("result", failedResult(AUTH_FAILURE, input.inputId))
          s.state = "idle"
          s.lastTurnOutcome = "error"
          host.turnEnded?.(s, input.text)
        },
      ]
    } else if (/approve/i.test(input.text)) {
      const command = "git push -u origin HEAD"
      const toolId = `toolu_${nanoid()}`
      steps = [
        start,
        () => {
          streamToolUse(r, current()!, toolId, command)
        },
        () => {
          // The denial, the approval card, then the tool result that tells the model.
          r.durable("permission_denied", permissionDenied(toolId))
          const approvalId = nanoid()
          s.approvals.push({ approvalId, toolUseId: toolId, command, state: "pending", createdAtUnixMs: host.now() })
          r.durable("approval_pending", { approval_id: approvalId, tool: "Bash", device: s.device, tool_use_id: toolId, display: command, reason: APPROVAL_REASON })
          r.durable("user", denialResult(toolId))
        },
        ...streamSteps(s, current, "I need your approval to push this branch."),
        finish("I need your approval to push this branch."),
      ]
    } else {
      steps = [
        start,
        ...streamSteps(s, current, "Let me look at the working tree first."),
        ...toolSteps(s, current, "git status --short", " M web/src/App.tsx\n?? notes.md", false),
        ...streamSteps(
          s,
          current,
          `One file changed and one is new. About your message ("${input.text}"): this is the mock backend, so that's all I can do here.`,
        ),
        finish("Done."),
      ]
    }
    turn = schedule(s, steps, input.inputId)
  }

  /** Ends the running turn early: what streamed so far stays, unsaved; a running tool fails. */
  function abortTurn(s: MockSession, outcome: "stopped" | "interrupted") {
    const turn = turns.get(s.sessionId)
    if (!turn) return
    turns.delete(s.sessionId)
    for (const timer of turn.timers) clearTimeout(timer)
    const r = recorder(s)
    if (outcome === "stopped") {
      const streaming = turn.streaming
      if (streaming && streaming.text !== "") r.text(streaming.text, streaming.messageId, true, streaming.startedBy)
      if (turn.tool) r.durable("user", interruptedResult(turn.tool))
      const note = turn.tool ? "[Request interrupted by user for tool use]" : "[Request interrupted by user]"
      r.durable("user", userMessage(nanoid(), [textBlock(note)], "user_interrupt"))
      if (streaming) {
        // The aborted response's stream ends after the note.
        r.ephemeral("assistant", streamEvent(streaming.messageId, { type: "content_block_stop", index: 0 }))
        r.ephemeral("assistant", messageDelta(streaming.messageId, null))
        r.ephemeral("assistant", streamEvent(streaming.messageId, { type: "message_stop" }, {
          abandoned_blocks: { api_message_id: streaming.messageId, from_block_index: 1 },
        }))
      }
      const reason = turn.tool ? "aborted_tools" : "aborted_streaming"
      r.durable("result", result("", turn.inputId, abortFailure(reason), reason))
      r.durable("turn_stopped", { turn_no: String(s.turns), outcome: "stopped" })
    } else {
      r.durable("turn_interrupted", { turn_no: String(s.turns), outcome: "interrupted" })
    }
    s.lastTurnOutcome = outcome
  }

  async function* watchSession({ sessionId, afterSeq, signal }: { sessionId: string; afterSeq: number; signal: AbortSignal }) {
    await host.delay()
    const s = host.own(sessionId)
    // A keepalive first, as the server opens every stream.
    const queue: RawSessionEvent[] = [
      { seq: 0, tsUnixMs: host.now(), kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false },
      ...s.events.filter((e) => e.seq > afterSeq),
    ]
    let wake: (() => void) | null = null
    const live: Live = (event) => {
      queue.push(event)
      wake?.()
    }
    const set = watchers.get(sessionId) ?? new Set()
    watchers.set(sessionId, set)
    set.add(live)
    try {
      while (!signal.aborted) {
        const next = queue.shift()
        if (next) {
          yield next
          continue
        }
        await new Promise<void>((resolve) => {
          wake = resolve
          signal.addEventListener("abort", () => resolve(), { once: true })
        })
        wake = null
      }
    } finally {
      set.delete(live)
    }
  }

  const service: MockSessionRuntime["service"] = {
    getSession: async ({ sessionId }) => {
      await host.delay()
      return detailsOf(host.own(sessionId))
    },
    sendMessage: async ({ sessionId, clientMsgId, text }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (text.trim() === "") throw new ApiError("invalid_argument", "The message is empty.", "text")
      const seen = s.events.find(
        (e) => e.kind === "input_accepted" && JSON.parse(e.payloadJson).client_msg_id === clientMsgId,
      )
      if (seen) {
        const payload = JSON.parse(seen.payloadJson) as { input_id: string; text: string }
        if (payload.text !== text) {
          throw new ApiError("already_exists", "This message id was sent with other text.", "client_msg_id")
        }
        return { inputId: payload.input_id, eventSeq: seen.seq }
      }
      if (s.state === "stopped") throw new ApiError("failed_precondition", "The session is stopped.")
      if (s.queue.length >= 32) throw new ApiError("resource_exhausted", "Too many messages wait for delivery.")
      const inputId = recorder(s).input(text, clientMsgId)
      s.queue.push({ inputId, text, approval: false })
      if (s.state === "idle") s.state = "queued"
      startNext(s)
      return { inputId, eventSeq: s.events.at(-1)!.seq }
    },
    watchSession,
    interrupt: async ({ sessionId }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (turns.has(sessionId)) abortTurn(s, "stopped")
      else if (s.state === "running") {
        // A turn the mock was seeded in the middle of: it stops at once.
        recorder(s).durable("turn_stopped", { turn_no: String(s.turns), outcome: "stopped" })
        s.lastTurnOutcome = "stopped"
      } else return
      s.state = "idle"
      startNext(s)
    },
    stopSession: async ({ sessionId }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (s.state === "stopped") return
      // A turn the mock runs, or one it was seeded in the middle of.
      const running = turns.has(s.sessionId) || s.state === "running"
      const turn = turns.get(s.sessionId)
      if (turn) for (const timer of turn.timers) clearTimeout(timer)
      turns.delete(s.sessionId)
      s.state = "stopped"
      s.queue = []
      for (const a of s.approvals) if (a.state === "pending") a.state = "denied"
      recorder(s).durable("session_state", { state: "stopped", reason: "user" })
      // The lease ends here (its live announcements first), then the turn.
      host.stopped?.(s)
      if (running) {
        recorder(s).durable("turn_interrupted", { turn_no: String(s.turns), outcome: "interrupted" })
        s.lastTurnOutcome = "interrupted"
      }
    },
    listApprovals: async ({ sessionId }) => {
      await host.delay()
      const s = host.own(sessionId)
      return s.approvals
        .filter((a) => a.state === "pending")
        .map(
          (a): SessionApproval => ({
            approvalId: a.approvalId,
            sessionId,
            toolUseId: a.toolUseId,
            tool: "Bash",
            device: s.device,
            display: a.command,
            reason: APPROVAL_REASON,
            expiresAtUnixMs: a.createdAtUnixMs + 10 * MINUTE,
          }),
        )
    },
    resolveApproval: async ({ approvalId, approve }) => {
      await host.delay()
      const s = host.sessions().find((x) => x.approvals.some((a) => a.approvalId === approvalId))
      if (!s) throw new ApiError("not_found", "Approval not found.")
      host.own(s.sessionId)
      const approval = s.approvals.find((a) => a.approvalId === approvalId)!
      if (approval.state !== "pending") return
      if (s.state === "stopped") throw new ApiError("failed_precondition", "The session is stopped.")
      approval.state = approve ? "approved" : "denied"
      recorder(s).durable("approval_resolved", { approval_id: approvalId, state: approval.state })
      if (approve) {
        s.queue.push({ inputId: nanoid(), text: approval.command, approval: true })
        startNext(s)
      }
    },
    recoverSession: async ({ sessionId }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (s.state !== "broken") throw new ApiError("failed_precondition", "The session is not broken.")
      s.state = s.queue.length > 0 ? "queued" : "idle"
      recorder(s).durable("transcript_gap_accepted", null)
      startNext(s)
    },
    updateSession: async ({ sessionId, title, providerId }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (title === undefined && providerId === undefined) {
        throw new ApiError("invalid_argument", "Nothing to change.", "session_id")
      }
      const trimmed = title?.trim()
      if (trimmed !== undefined && (trimmed === "" || [...trimmed].length > 200)) {
        throw new ApiError("invalid_argument", "Invalid title.", "title")
      }
      if (providerId !== undefined) {
        if (s.state === "running" || turns.has(s.sessionId)) {
          throw new ApiError("failed_precondition", "A turn is running.")
        }
        if (s.state === "stopped") throw new ApiError("failed_precondition", "The session is stopped.")
        host.setProvider?.(s, providerId)
      }
      if (trimmed !== undefined) s.title = trimmed
      startNext(s)
      return detailsOf(s)
    },
  }

  return {
    service,
    record(s, kind, payload, durable, tsUnixMs) {
      const r = recorder(s)
      if (durable) r.durable(kind, payload, tsUnixMs)
      else r.ephemeral(kind, payload, tsUnixMs)
    },
    wake: startNext,
    leaseLost(s) {
      if (turns.has(s.sessionId)) abortTurn(s, "interrupted")
      else if (s.state === "running") {
        recorder(s).durable("turn_interrupted", { turn_no: String(s.turns), outcome: "interrupted" })
        s.lastTurnOutcome = "interrupted"
      } else return
      s.state = "idle"
      startNext(s)
    },
  }
}
