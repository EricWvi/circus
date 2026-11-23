// Builds the session view from the control plane's raw session events. Pure:
// `applyEvents` returns a new transcript and leaves the old one as it was.
//
// Event ids are derived from the events themselves (input, message and tool
// ids, or the seq of a marker), so a replay after a reconnect produces the
// same ids and React keeps its elements.

import type { RawSessionEvent, WorkspaceStatus } from "@/api/types"
import { queueReasonFromPayload, type LiveQueueReason } from "@/lib/queue-reason"
import { workerDisconnectedAtFromPayload, type LiveWorkerLink } from "@/lib/worker-link"
import { workspaceFromPayload, type LiveWorkspace } from "@/lib/workspace"

import { NO_RESULT_TOOL_ERROR, settleEvents } from "../settle"
import type {
  AssistantTextEvent,
  NoteKind,
  ReasoningEvent,
  SessionEvent,
  ToolCallEvent,
} from "../types"

export interface Transcript {
  /** The highest durable seq applied; the next stream starts after it. */
  readonly lastSeq: number
  /** View events, oldest first. */
  readonly events: readonly SessionEvent[]
  /** A turn has started and its end hasn't arrived yet. */
  readonly turnOpen: boolean
  /** The number of the latest turn that started, 0 before any. */
  readonly turnNo: number
  /** The latest workspace state the stream brought (durable or live); null before any. */
  readonly workspace: LiveWorkspace | null
  /** The latest queue reason the stream announced (live only); null before any. */
  readonly queue: LiveQueueReason | null
  /** The latest worker disconnect time the stream announced (live only); null before any. */
  readonly worker: LiveWorkerLink | null
  /** Bookkeeping; opaque to callers. */
  readonly internal: Internal
}

interface Internal {
  order: string[]
  byId: Map<string, SessionEvent>
  /** Input id → the user message's event id. */
  inputs: Map<string, string>
  /** Approval id → the tool call's event id. */
  approvals: Map<string, string>
  /**
   * API message id → its durable blocks seen so far, not counting
   * truncated ones: a replay can't tell which message a truncated block
   * belonged to, so block ids never count them.
   */
  blocks: Map<string, number>
  /** API message id → its blocks that arrived truncated while it streamed. */
  skipped: Map<string, number>
  /** Tool calls the permission system denied; their error result is not shown. */
  denied: Set<string>
  /** When a streamed block started, by event id (for thinking time). */
  started: Map<string, number>
  /** The API message being streamed, from its message_start. */
  streaming: string | null
  /** The workspace's latest recorded failure, shown once a later change supersedes it. */
  failure: {
    seq: number
    at: string
    status: WorkspaceStatus
    /** The view event it came after, where its line goes; null at the start. */
    after: string | null
  } | null
}

export const emptyTranscript: Transcript = {
  lastSeq: 0,
  events: [],
  turnOpen: false,
  turnNo: 0,
  workspace: null,
  queue: null,
  worker: null,
  internal: {
    order: [],
    byId: new Map(),
    inputs: new Map(),
    approvals: new Map(),
    blocks: new Map(),
    skipped: new Map(),
    denied: new Set(),
    started: new Map(),
    streaming: null,
    failure: null,
  },
}

/** The event id of a user message, optimistic or from the stream. */
export const messageEventId = (clientMsgId: string) => `msg:${clientMsgId}`

const toolEventId = (toolUseId: string) => `tool:${toolUseId}`

/** Kinds whose content lives in the payload; truncated, they show a placeholder. */
const contentKinds = new Set<RawSessionEvent["kind"]>(["assistant", "user", "input_accepted", "error"])

type Json = Record<string, unknown>

const isObject = (v: unknown): v is Json => typeof v === "object" && v !== null && !Array.isArray(v)
const str = (v: unknown): string => (typeof v === "string" ? v : "")

function parse(json: string): Json | null {
  try {
    const value: unknown = JSON.parse(json)
    return isObject(value) ? value : null
  } catch {
    return null
  }
}

/** The text of a tool result's or message's content: a string or blocks. */
function contentText(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content
    .map((block) => {
      if (!isObject(block)) return ""
      if (block.type === "text") return str(block.text)
      if (block.type === "image") return "[image]"
      return ""
    })
    .filter((text) => text !== "")
    .join("\n")
}

/** The text blocks of a note's content, or its string content as it is. */
function noteText(content: unknown): string {
  if (typeof content === "string") return content
  if (!Array.isArray(content)) return ""
  return content
    .filter((block): block is Json => isObject(block) && block.type === "text")
    .map((block) => str(block.text))
    .filter((text) => text !== "")
    .join("\n")
}

/** Notes the view shows, by their `circus_note` kind (docs/wire.md). */
const shownNotes = new Set<string>([
  "previous_turn_interrupted",
  "recovered_call",
  "call_notice",
  "job_notice",
  "lost_bgtask",
  "history_missing",
] satisfies NoteKind[])

/**
 * Notes the view already says in its own way: the approval card shows the
 * retry an approval starts, and "You stopped this turn" the interrupt.
 */
const hiddenNotes = new Set(["approval_retry", "user_interrupt"])

/**
 * Applies a batch of raw events in order. Durable events at or below
 * `lastSeq` are skipped. An event the reducer can't handle is skipped too,
 * without losing the rest of the batch.
 */
export function applyEvents(transcript: Transcript, batch: readonly RawSessionEvent[]): Transcript {
  if (batch.length === 0) return transcript
  const t = new Builder(transcript)
  for (const event of batch) {
    try {
      t.apply(event)
    } catch (error) {
      console.error("session event skipped", event.seq, event.kind, error)
    }
  }
  return t.build()
}

/** An ISO 8601 time; one out of range (the server's clock, a bad value) reads as now. */
function isoTime(unixMs: number): string {
  const date = new Date(unixMs)
  return Number.isNaN(date.getTime()) ? new Date().toISOString() : date.toISOString()
}

const isStreaming = (e: SessionEvent) => (e.kind === "assistant_text" || e.kind === "reasoning") && e.streaming === true

/** A message of a subagent (Task) belongs to its tool call, not to the main conversation. */
const isSubagent = (p: Json) => typeof p.parent_tool_use_id === "string" && p.parent_tool_use_id !== ""

/**
 * Drops what was streamed live and never became durable: after a reconnect
 * nothing replays it, and the durable events take its place.
 */
export function withoutStreaming(transcript: Transcript): Transcript {
  const t = new Builder(transcript)
  t.dropStreaming()
  t.internal.streaming = null
  return t.build()
}

class Builder {
  internal: Internal
  lastSeq: number
  turnOpen: boolean
  turnNo: number
  workspace: LiveWorkspace | null
  queue: LiveQueueReason | null
  worker: LiveWorkerLink | null
  private changed = false

  constructor(private readonly base: Transcript) {
    const i = base.internal
    this.internal = {
      order: [...i.order],
      byId: new Map(i.byId),
      inputs: new Map(i.inputs),
      approvals: new Map(i.approvals),
      blocks: new Map(i.blocks),
      skipped: new Map(i.skipped),
      denied: new Set(i.denied),
      started: new Map(i.started),
      streaming: i.streaming,
      failure: i.failure,
    }
    this.lastSeq = base.lastSeq
    this.turnOpen = base.turnOpen
    this.turnNo = base.turnNo
    this.workspace = base.workspace
    this.queue = base.queue
    this.worker = base.worker
  }

  build(): Transcript {
    const { order, byId } = this.internal
    return {
      lastSeq: this.lastSeq,
      turnOpen: this.turnOpen,
      turnNo: this.turnNo,
      workspace: this.workspace,
      queue: this.queue,
      worker: this.worker,
      events: this.changed ? order.map((id) => byId.get(id)!) : this.base.events,
      internal: this.internal,
    }
  }

  private get(id: string) {
    return this.internal.byId.get(id)
  }

  /**
   * Adds a durable event, or one that took the place of what streamed under
   * its id. Durable events keep the order they were recorded in, ahead of
   * blocks still streaming, so a replay shows them in the same order.
   */
  private add(event: SessionEvent) {
    const { order, byId } = this.internal
    const existing = byId.get(event.id)
    if (existing && !isStreaming(existing)) {
      byId.set(event.id, event)
      this.changed = true
      return
    }
    if (existing) order.splice(order.indexOf(event.id), 1)
    const tail = order.findIndex((id) => isStreaming(byId.get(id)!))
    order.splice(tail < 0 ? order.length : tail, 0, event.id)
    byId.set(event.id, event)
    this.changed = true
  }

  /**
   * Adds a durable event where an earlier one would have gone: right after
   * `after` (at the start when null), or at the end of what is recorded
   * when `after` is no longer in the view.
   */
  private insertAfter(after: string | null, event: SessionEvent) {
    const { order, byId } = this.internal
    if (byId.has(event.id)) return
    const i = after === null ? 0 : order.indexOf(after) + 1
    if (after !== null && i === 0) return this.add(event)
    order.splice(i, 0, event.id)
    byId.set(event.id, event)
    this.changed = true
  }

  /** Adds a block that starts streaming, after everything else. */
  private addStreaming(event: AssistantTextEvent | ReasoningEvent) {
    this.internal.order.push(event.id)
    this.internal.byId.set(event.id, event)
    this.changed = true
  }

  private put(event: SessionEvent) {
    this.internal.byId.set(event.id, event)
    this.changed = true
  }

  private remove(ids: Set<string>) {
    if (ids.size === 0) return
    this.internal.order = this.internal.order.filter((id) => !ids.has(id))
    for (const id of ids) this.internal.byId.delete(id)
    this.changed = true
  }

  dropStreaming() {
    const ids = new Set<string>()
    for (const [id, e] of this.internal.byId) {
      if ((e.kind === "assistant_text" || e.kind === "reasoning") && e.streaming) ids.add(id)
    }
    this.remove(ids)
  }

  /** Ends what nothing will finish: running tool calls fail, streamed text stays unsaved. */
  private settle(keepStreamedText: boolean, toolError?: string) {
    if (!keepStreamedText) this.dropStreaming()
    const events = this.internal.order.map((id) => this.get(id)!)
    const marked = events.map((e): SessionEvent => {
      if (e.kind === "assistant_text" && e.streaming) return { ...e, streaming: false, unsaved: true }
      if (e.kind === "reasoning" && e.streaming) return { ...e, streaming: false }
      return e
    })
    settleEvents(marked, toolError).forEach((e, i) => {
      if (e !== events[i]) this.put(e)
    })
    this.internal.streaming = null
  }

  apply(raw: RawSessionEvent) {
    if (raw.seq > 0) {
      if (raw.seq <= this.lastSeq) return
      this.lastSeq = raw.seq
    }
    if (raw.kind === "unspecified") return
    const at = isoTime(raw.tsUnixMs)
    if (raw.truncated && contentKinds.has(raw.kind)) {
      if (raw.seq > 0) this.truncated(raw, at)
      return
    }
    const p = parse(raw.payloadJson) ?? {}
    switch (raw.kind) {
      case "input_accepted":
        return this.inputAccepted(p, at)
      case "turn_started": {
        this.turnOpen = true
        const no = Number(str(p.turn_no))
        if (Number.isSafeInteger(no) && no > this.turnNo) this.turnNo = no
        return
      }
      case "user":
        return this.user(p, raw.seq, at)
      case "assistant":
        if (raw.ephemeral) return this.partial(p, at, raw.tsUnixMs)
        return this.assistant(p, at, raw.tsUnixMs)
      case "permission_denied":
        return this.permissionDenied(p)
      case "approval_pending":
        return this.approvalPending(p, at, raw.tsUnixMs)
      case "approval_resolved":
        return this.approvalResolved(p)
      case "result":
        // The turn is over: nothing answers a tool call still running in it
        // (its result may have been too large for the view).
        this.turnOpen = false
        this.settle(false, NO_RESULT_TOOL_ERROR)
        return
      case "turn_stopped":
        this.turnOpen = false
        this.settle(false)
        this.add({ kind: "turn_stopped", id: `stopped:${raw.seq}`, at })
        return
      case "turn_interrupted":
        this.turnOpen = false
        this.settle(true)
        this.add({ kind: "turn_interrupted", id: `interrupted:${raw.seq}`, at })
        return
      case "session_state":
        if (p.state === "stopped") {
          this.turnOpen = false
          this.settle(false)
        }
        return
      case "transcript_gap_accepted":
        this.add({ kind: "history_gap", id: `gap:${raw.seq}`, at })
        return
      case "error":
        if (raw.seq > 0) this.add({ kind: "turn_error", id: `error:${raw.seq}`, at, message: str(p.message) })
        return
      case "workspace":
        return this.workspaceChanged(p, raw, at)
      case "queue":
        // Live only, applied in the order received: the latest one counts.
        this.queue = { reason: queueReasonFromPayload(p), receivedAtMs: raw.receivedAtMs ?? 0 }
        return
      case "worker":
        // Live only, like a queue reason: the latest one received counts.
        this.worker = { disconnectedAtUnixMs: workerDisconnectedAtFromPayload(p), receivedAtMs: raw.receivedAtMs ?? 0 }
        return
      default:
        // Progress, background tasks, notices and kinds this view doesn't know.
        return
    }
  }

  /**
   * The session's folder changed. The latest state, live or recorded, is
   * kept for the banner, which says what is under way. The conversation
   * keeps outcomes only: a line once the folder is ready, and a failure's
   * line once a later recorded change (a retry) supersedes it; until then
   * the banner says it. Events of the same time are the same change, so the
   * later one in the stream wins.
   */
  private workspaceChanged(p: Json, raw: RawSessionEvent, at: string) {
    const status = workspaceFromPayload(p, raw.tsUnixMs)
    if (!status) return
    const durable = raw.seq > 0
    const previous = this.workspace
    if (!previous || raw.tsUnixMs >= previous.tsUnixMs) {
      // An attempt starts when the workspace moves to preparing (from
      // pending: a new attempt always waits first). Anything said while it
      // prepares, such as moving to the background, belongs to the same
      // attempt, whether sent live or recorded.
      let attemptStartedAtUnixMs = 0
      if (status.state === "preparing") {
        const same = previous?.status.state === "preparing" && previous.attemptStartedAtUnixMs > 0
        if (same) attemptStartedAtUnixMs = previous.attemptStartedAtUnixMs
        else if (durable) attemptStartedAtUnixMs = raw.tsUnixMs
      }
      this.workspace = { status, tsUnixMs: raw.tsUnixMs, attemptStartedAtUnixMs }
    }
    if (!durable) return
    const failure = this.internal.failure
    if (failure && status.state !== "failed") {
      this.insertAfter(failure.after, {
        kind: "workspace",
        id: `workspace:${failure.seq}`,
        at: failure.at,
        status: failure.status,
      })
    }
    this.internal.failure =
      status.state === "failed"
        ? { seq: raw.seq, at, status, after: this.internal.order.findLast((id) => !isStreaming(this.get(id)!)) ?? null }
        : null
    if (status.state === "ready") this.add({ kind: "workspace", id: `workspace:${raw.seq}`, at, status })
  }

  /**
   * A durable event whose payload was too large to keep. A truncated
   * assistant block of the message being streamed replaces its streamed
   * copy, and the blocks streamed after it are numbered without it, as a
   * replay numbers them.
   */
  private truncated(raw: RawSessionEvent, at: string) {
    const placeholder: SessionEvent = { kind: "truncated", id: `truncated:${raw.seq}`, at }
    const streaming = this.internal.streaming
    if (raw.kind === "assistant" && streaming !== null) {
      const blockId = `${streaming}:${this.internal.blocks.get(streaming) ?? 0}`
      this.internal.skipped.set(streaming, (this.internal.skipped.get(streaming) ?? 0) + 1)
      this.internal.started.delete(blockId)
      if (this.get(blockId)) this.remove(new Set([blockId]))
    }
    this.add(placeholder)
  }

  /** The id of the streamed block at `index` of the message being streamed. */
  private streamId(index: number): string {
    const m = this.internal.streaming!
    return `${m}:${index - (this.internal.skipped.get(m) ?? 0)}`
  }

  private inputAccepted(p: Json, at: string) {
    const inputId = str(p.input_id)
    const clientMsgId = str(p.client_msg_id) || inputId
    if (!clientMsgId) return
    const id = messageEventId(clientMsgId)
    if (inputId) this.internal.inputs.set(inputId, id)
    if (this.get(id)) return
    this.add({ kind: "user_message", id, at, text: str(p.text), clientMsgId, delivery: "accepted" })
  }

  private user(p: Json, seq: number, at: string) {
    const message = isObject(p.message) ? p.message : {}
    const content = message.content
    const input = this.internal.inputs.get(str(p.uuid))
    if (input) {
      // The worker handed the user's own message to the agent.
      const event = this.get(input)
      if (event?.kind === "user_message" && event.delivery !== "delivered") {
        this.put({ ...event, delivery: "delivered" })
      }
      return
    }
    // A subagent's prompt and tool results stay inside its Task call.
    if (isSubagent(p)) return
    const results = Array.isArray(content)
      ? content.filter((block): block is Json => isObject(block) && block.type === "tool_result")
      : []
    for (const block of results) this.toolResult(block)
    if (results.length > 0) return
    // What the worker tells the agent (recovery, notices, gaps) is a note.
    // An unmarked message is neither: the user's own messages enter the
    // view as INPUT_ACCEPTED.
    const note = str(p.circus_note)
    if (!note && p.isSynthetic !== true) return
    if (hiddenNotes.has(note)) return
    const uuid = str(p.uuid)
    if (!uuid && seq <= 0) return
    this.add({
      kind: "note",
      id: uuid ? `note:${uuid}` : `note:${seq}`,
      at,
      noteKind: shownNotes.has(note) ? (note as NoteKind) : "other",
      text: noteText(content),
    })
  }

  private toolResult(block: Json) {
    const toolUseId = str(block.tool_use_id)
    const id = toolEventId(toolUseId)
    const event = this.get(id)
    if (event?.kind !== "tool_call") return
    // The permission system's rejection is for the agent; the approval says it.
    if (this.internal.denied.has(id)) return
    const text = contentText(block.content)
    const next: ToolCallEvent =
      block.is_error === true
        ? { ...event, status: "error", error: text, output: undefined }
        : { ...event, status: "completed", output: text }
    this.put(next)
  }

  /** A live `stream_event`: text and thinking appear as they stream. */
  private partial(p: Json, at: string, ts: number) {
    if (p.type !== "stream_event" || !isObject(p.event) || isSubagent(p)) return
    const e = p.event
    switch (e.type) {
      case "message_start":
        this.internal.streaming = isObject(e.message) ? str(e.message.id) || null : null
        return
      case "content_block_start": {
        if (this.internal.streaming === null || typeof e.index !== "number") return
        const block = isObject(e.content_block) ? e.content_block : {}
        const id = this.streamId(e.index)
        if (this.get(id)) return
        if (block.type === "text") {
          this.addStreaming({ kind: "assistant_text", id, at, text: str(block.text), streaming: true })
        } else if (block.type === "thinking") {
          this.addStreaming({ kind: "reasoning", id, at, text: str(block.thinking), streaming: true })
        } else {
          return
        }
        this.internal.started.set(id, ts)
        return
      }
      case "content_block_delta": {
        if (this.internal.streaming === null || typeof e.index !== "number" || !isObject(e.delta)) return
        const event = this.get(this.streamId(e.index))
        const delta = e.delta
        if (event?.kind === "assistant_text" && event.streaming && delta.type === "text_delta") {
          this.put({ ...event, text: event.text + str(delta.text) })
        } else if (event?.kind === "reasoning" && event.streaming && delta.type === "thinking_delta") {
          this.put({ ...event, text: event.text + str(delta.thinking) })
        }
        return
      }
      default:
        return
    }
  }

  /** A durable assistant message: completed blocks, which replace their streamed copy. */
  private assistant(p: Json, at: string, ts: number) {
    if (isSubagent(p)) return
    const message = isObject(p.message) ? p.message : {}
    const messageId = str(message.id)
    const blocks = Array.isArray(message.content) ? message.content : []
    const aborted = p.aborted === true
    for (const block of blocks) {
      const n = this.internal.blocks.get(messageId) ?? 0
      this.internal.blocks.set(messageId, n + 1)
      if (!isObject(block)) continue
      const blockId = `${messageId}:${n}`
      const streamed = this.get(blockId)
      const startedAt = this.internal.started.get(blockId)
      this.internal.started.delete(blockId)
      switch (block.type) {
        case "text": {
          const event: AssistantTextEvent = { kind: "assistant_text", id: blockId, at, text: str(block.text) }
          if (aborted) event.unsaved = true
          this.add(event)
          break
        }
        case "thinking": {
          const event: ReasoningEvent = { kind: "reasoning", id: blockId, at, text: str(block.thinking) }
          if (startedAt !== undefined) event.durationSeconds = Math.max(1, Math.round((ts - startedAt) / 1000))
          this.add(event)
          break
        }
        case "tool_use": {
          if (streamed) this.remove(new Set([blockId]))
          const id = toolEventId(str(block.id))
          if (this.get(id)) break
          this.add({
            kind: "tool_call",
            id,
            at,
            tool: str(block.name),
            input: isObject(block.input) ? block.input : {},
            status: "running",
          })
          break
        }
        default:
          if (streamed) this.remove(new Set([blockId]))
      }
    }
  }

  private permissionDenied(p: Json) {
    const id = toolEventId(str(p.tool_use_id))
    const event = this.get(id)
    if (event?.kind !== "tool_call") return
    this.internal.denied.add(id)
    if (event.status !== "awaiting_approval") this.put({ ...event, status: "denied", error: undefined })
  }

  private approvalPending(p: Json, at: string, ts: number) {
    const approvalId = str(p.approval_id)
    if (!approvalId) return
    const toolUseId = str(p.tool_use_id)
    const id = toolEventId(toolUseId || approvalId)
    const approval = { approvalId, reason: str(p.reason), display: str(p.display), requestedAtUnixMs: ts }
    const event = this.get(id)
    this.internal.approvals.set(approvalId, id)
    this.internal.denied.add(id)
    if (event?.kind === "tool_call") {
      if (event.approval?.decision) return
      this.put({ ...event, status: "awaiting_approval", error: undefined, approval })
      return
    }
    // The tool use itself isn't in the view (it was dropped as too large).
    this.add({
      kind: "tool_call",
      id,
      at,
      tool: str(p.tool),
      input: { command: approval.display },
      status: "awaiting_approval",
      approval,
    })
  }

  private approvalResolved(p: Json) {
    const id = this.internal.approvals.get(str(p.approval_id))
    const event = id ? this.get(id) : undefined
    if (event?.kind !== "tool_call" || !event.approval) return
    const state = p.state
    if (state !== "approved" && state !== "denied" && state !== "expired") return
    // An approved call still didn't run: the agent retries it as a call of
    // its own, which shows on its own card.
    this.put({ ...event, status: "denied", approval: { ...event.approval, decision: state } })
  }
}
