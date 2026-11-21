// The mock's sessions: in memory, for this tab only. A message gets a short
// scripted reply, emitted as the raw events the control plane streams (the
// Claude Agent SDK message shapes the Go fake worker publishes), so the
// pages and the transcript reducer run the same code as against a server:
// streamed text, one Bash call, and an approval when the message asks for
// one ("approve" anywhere in it).

import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import type {
  RawSessionEvent,
  SessionApproval,
  SessionDetails,
  SessionEventKind,
  SessionService,
  SessionSummary,
} from "@/api/types"
import { viewState, type ServerSessionState, type TurnOutcome } from "@/lib/session-state"
import { titleFromMessage } from "@/lib/title"
import type { SeedSession } from "@/mock/data"

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
const INTERRUPTED_TOOL =
  "The tool call was interrupted before a result was received. It may or may not have completed on the server — verify before assuming it succeeded, and retry if needed."

// --- SDK message shapes -------------------------------------------------

const envelope = (type: string, uuid: string) => ({ type, uuid, session_id: "mock", parent_tool_use_id: null })
const userMessage = (uuid: string, content: unknown, synthetic = false) => ({
  ...envelope("user", uuid),
  message: { role: "user", content },
  ...(synthetic ? { isSynthetic: true } : {}),
})
const assistantMessage = (messageId: string, block: object, aborted = false) => ({
  ...envelope("assistant", nanoid()),
  message: { id: messageId, type: "message", role: "assistant", model: "claude-mock", content: [block] },
  ...(aborted ? { aborted: true } : {}),
})
const streamEvent = (event: object) => ({ ...envelope("stream_event", nanoid()), event })
const textBlock = (text: string) => ({ type: "text", text, citations: null })
const toolUse = (id: string, command: string) => ({ type: "tool_use", id, name: "Bash", input: { command } })
const toolResult = (id: string, content: unknown, isError: boolean) => [
  { type: "tool_result", tool_use_id: id, content, is_error: isError },
]
const result = (text: string, failure?: string) => ({
  ...envelope("result", nanoid()),
  subtype: failure ? "error_during_execution" : "success",
  is_error: failure !== undefined,
  ...(failure ? { errors: [failure], terminal_reason: "aborted_streaming" } : { result: text }),
})

/** Appends events to a session as the control plane records them. */
export class Recorder {
  constructor(
    private readonly session: MockSession,
    private readonly now: () => number,
    private readonly emit: (event: RawSessionEvent) => void = () => {},
  ) {}

  durable(kind: SessionEventKind, payload: object | null) {
    const s = this.session
    const event: RawSessionEvent = {
      seq: (s.events.at(-1)?.seq ?? 0) + 1,
      tsUnixMs: this.now(),
      kind,
      payloadJson: payload === null ? "" : JSON.stringify(payload),
      ephemeral: false,
      truncated: false,
    }
    s.events.push(event)
    s.lastActivityAtUnixMs = Math.max(s.lastActivityAtUnixMs, event.tsUnixMs)
    this.emit(event)
  }

  ephemeral(kind: SessionEventKind, payload: object) {
    this.emit({ seq: 0, tsUnixMs: this.now(), kind, payloadJson: JSON.stringify(payload), ephemeral: true, truncated: false })
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

  text(text: string, messageId = `msg_${nanoid()}`, aborted = false) {
    this.durable("assistant", assistantMessage(messageId, textBlock(text), aborted))
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
  }
  let clock = at - 40 * MINUTE
  const r = new Recorder(session, () => (clock += 1_000))
  const exchange = (text: string, reply: string) => {
    const inputId = r.input(text)
    r.turnStarted()
    r.durable("user", userMessage(inputId, text))
    r.text(reply)
    r.durable("result", result(reply))
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
      r.durable("assistant", assistantMessage(`msg_${nanoid()}`, toolUse(toolId, command)))
      r.durable("permission_denied", { type: "system", subtype: "permission_denied", tool_name: "Bash", tool_use_id: toolId, message: "Denied." })
      r.durable("user", userMessage(nanoid(), toolResult(toolId, "Denied.", true)))
      const approvalId = nanoid()
      session.approvals.push({ approvalId, toolUseId: toolId, command, state: "pending", createdAtUnixMs: clock })
      r.durable("approval_pending", { approval_id: approvalId, tool: "bash", device: seed.device, tool_use_id: toolId, display: command, reason: APPROVAL_REASON })
      r.text("I need your approval to push the branch.")
      r.durable("result", result(""))
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
    case "broken": {
      const inputId = r.input(seed.followUp ?? "Go on")
      r.turnStarted()
      r.durable("user", userMessage(inputId, seed.followUp ?? "Go on"))
      r.durable("error", { message: "transcript mirror failure" })
      r.durable("result", result("", "transcript mirror failure"))
      session.state = "broken"
      session.lastTurnOutcome = "error"
      break
    }
    case "stopped":
      r.durable("session_state", { state: "stopped", reason: "user" })
      session.state = "stopped"
      break
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
  }
}

export const detailsOf = (s: MockSession): SessionDetails => ({ ...summaryOf(s), device: s.device, cwd: s.cwd })

/** What the session service needs from the rest of the mock backend. */
export interface MockSessionHost {
  sessions(): MockSession[]
  /** The session if it belongs to a project of the signed-in user; throws otherwise. */
  own(sessionId: string): MockSession
  /** Runs a handler after the simulated network delay, like the other mock calls. */
  delay(): Promise<void>
  now(): number
  timing: MockSessionTiming
}

type Live = (event: RawSessionEvent) => void

/** The session RPCs besides listing and creating, which need the projects. */
export function createMockSessionRuntime(host: MockSessionHost): Omit<SessionService, "listSessions" | "createSession"> {
  const watchers = new Map<string, Set<Live>>()
  /** Pending steps of each session's running turn. */
  const turns = new Map<string, Turn>()

  interface Turn {
    timers: ReturnType<typeof setTimeout>[]
    /** Text streamed so far in the current block, with its message id. */
    streaming: { messageId: string; text: string } | null
    /** A Bash call waiting for its result. */
    tool: string | null
  }

  const recorder = (s: MockSession) =>
    new Recorder(s, host.now, (event) => {
      for (const live of watchers.get(s.sessionId) ?? []) live(event)
    })

  /** Runs the steps one after another, `stepMs` apart (a step may ask for longer). */
  function schedule(s: MockSession, steps: (() => number | void)[]) {
    const turn: Turn = { timers: [], streaming: null, tool: null }
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

  /** Streamed text: live chunks, then the durable block. Returns the step and its duration. */
  function streamSteps(s: MockSession, turn: () => Turn | undefined, text: string): (() => number | void)[] {
    const r = recorder(s)
    const messageId = `msg_${nanoid()}`
    const words = text.split(/(?<=\s)/)
    const chunks: string[] = []
    for (let i = 0; i < words.length; i += 3) chunks.push(words.slice(i, i + 3).join(""))
    return [
      () => {
        r.ephemeral("assistant", streamEvent({ type: "message_start", message: { id: messageId, content: [] } }))
        r.ephemeral("assistant", streamEvent({ type: "content_block_start", index: 0, content_block: textBlock("") }))
        turn()!.streaming = { messageId, text: "" }
        return host.timing.chunkMs
      },
      ...chunks.map((chunk) => () => {
        r.ephemeral("assistant", streamEvent({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: chunk } }))
        turn()!.streaming!.text += chunk
        return host.timing.chunkMs
      }),
      () => {
        r.ephemeral("assistant", streamEvent({ type: "content_block_stop", index: 0 }))
        r.ephemeral("assistant", streamEvent({ type: "message_stop" }))
        r.text(text, messageId)
        turn()!.streaming = null
      },
    ]
  }

  function toolSteps(s: MockSession, turn: () => Turn | undefined, command: string, output: string) {
    const r = recorder(s)
    const toolId = `toolu_${nanoid()}`
    return [
      () => {
        r.durable("assistant", assistantMessage(`msg_${nanoid()}`, toolUse(toolId, command)))
        turn()!.tool = toolId
        return host.timing.stepMs * 2
      },
      () => {
        r.durable("user", userMessage(nanoid(), toolResult(toolId, [{ type: "text", text: output }], false)))
        turn()!.tool = null
      },
    ]
  }

  /** Starts a turn for the next queued input, if the session can run one. */
  function startNext(s: MockSession) {
    if (turns.has(s.sessionId) || s.state === "stopped" || s.state === "broken") return
    const input = s.queue.shift()
    if (!input) {
      if (s.state === "running" || s.state === "queued") s.state = "idle"
      return
    }
    const r = recorder(s)
    let turn: Turn | undefined
    const current = () => turn
    const finish = (text: string) => () => {
      r.durable("result", result(text))
      s.state = "idle"
      s.lastTurnOutcome = "completed"
    }
    const start = () => {
      r.turnStarted()
      r.durable("user", userMessage(input.inputId, input.text, input.approval))
    }
    let steps: (() => number | void)[]
    if (input.approval) {
      const command = input.text
      steps = [
        start,
        ...toolSteps(s, current, command, "branch 'fix' set up to track 'origin/fix'."),
        ...streamSteps(s, current, "Pushed. The branch is on the remote now."),
        finish("Pushed."),
      ]
    } else if (/approve/i.test(input.text)) {
      const command = "git push -u origin HEAD"
      const toolId = `toolu_${nanoid()}`
      steps = [
        start,
        () => {
          r.durable("assistant", assistantMessage(`msg_${nanoid()}`, toolUse(toolId, command)))
        },
        () => {
          r.durable("permission_denied", { type: "system", subtype: "permission_denied", tool_name: "Bash", tool_use_id: toolId, message: "Denied." })
          r.durable("user", userMessage(nanoid(), toolResult(toolId, "Denied.", true)))
          const approvalId = nanoid()
          s.approvals.push({ approvalId, toolUseId: toolId, command, state: "pending", createdAtUnixMs: host.now() })
          r.durable("approval_pending", { approval_id: approvalId, tool: "bash", device: s.device, tool_use_id: toolId, display: command, reason: APPROVAL_REASON })
        },
        ...streamSteps(s, current, "I need your approval to push this branch."),
        finish("I need your approval to push this branch."),
      ]
    } else {
      steps = [
        start,
        ...streamSteps(s, current, "Let me look at the working tree first."),
        ...toolSteps(s, current, "git status --short", " M web/src/App.tsx\n?? notes.md"),
        ...streamSteps(
          s,
          current,
          `One file changed and one is new. About your message ("${input.text}"): this is the mock backend, so that's all I can do here.`,
        ),
        finish("Done."),
      ]
    }
    turn = schedule(s, steps)
  }

  /** Ends the running turn early: what streamed so far stays, unsaved; a running tool fails. */
  function abortTurn(s: MockSession, outcome: "stopped" | "interrupted") {
    const turn = turns.get(s.sessionId)
    if (!turn) return
    turns.delete(s.sessionId)
    for (const timer of turn.timers) clearTimeout(timer)
    const r = recorder(s)
    if (outcome === "stopped") {
      if (turn.streaming && turn.streaming.text !== "") r.text(turn.streaming.text, turn.streaming.messageId, true)
      if (turn.tool) r.durable("user", userMessage(nanoid(), toolResult(turn.tool, INTERRUPTED_TOOL, true)))
      r.durable("user", userMessage(nanoid(), [textBlock("[Request interrupted by user]")], true))
      r.durable("result", result("", "aborted"))
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

  return {
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
      if (!turns.has(sessionId)) return
      abortTurn(s, "stopped")
      s.state = "idle"
      startNext(s)
    },
    stopSession: async ({ sessionId }) => {
      await host.delay()
      const s = host.own(sessionId)
      if (s.state === "stopped") return
      const running = turns.has(s.sessionId)
      const turn = turns.get(s.sessionId)
      if (turn) for (const timer of turn.timers) clearTimeout(timer)
      turns.delete(s.sessionId)
      s.state = "stopped"
      recorder(s).durable("session_state", { state: "stopped", reason: "user" })
      if (running) {
        recorder(s).durable("turn_interrupted", { turn_no: String(s.turns), outcome: "interrupted" })
        s.lastTurnOutcome = "interrupted"
      }
      s.queue = []
      for (const a of s.approvals) if (a.state === "pending") a.state = "denied"
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
            tool: "bash",
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
    updateSession: async ({ sessionId, title }) => {
      await host.delay()
      const s = host.own(sessionId)
      const trimmed = title.trim()
      if (trimmed === "" || [...trimmed].length > 200) throw new ApiError("invalid_argument", "Invalid title.", "title")
      s.title = trimmed
      return detailsOf(s)
    },
  }
}
