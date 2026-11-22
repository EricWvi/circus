import { create } from "@bufbuild/protobuf";
import { SessionEvent_Kind as Kind } from "@circus/proto/circus/v1/session_pb";
import { PublishEventsSchema, PublishEvents_EventSchema, type PublishEvents_Event } from "@circus/proto/circus/v1/worker_link_pb";
import type { Logger } from "../log.js";
import { RejectedError, type SessionOps } from "./ops.js";

export { Kind };

// Payloads of the events the worker writes itself. Their shapes equal the
// Go fake worker's (internal/testutil/fakeworker/testdata/worker_events.json,
// compared by test/events.test.ts).

/** circus_note kinds of messages the worker writes for the model (docs/wire.md). */
export type NoteKind = "approval_retry" | "user_interrupt" | "previous_turn_interrupted" | "lost_bgtask" | "history_missing";

export type TurnOriginName = "input" | "notification" | "cli";

export const turnStartedPayload = (turnNo: bigint, origin: TurnOriginName) => ({ turn_no: turnNo.toString(), origin });

export const approvalPendingPayload = (a: {
  approvalId: string; tool: string; device: string; toolUseId: string; display: string; reason: string;
}) => ({
  approval_id: a.approvalId, tool: a.tool, device: a.device, tool_use_id: a.toolUseId, display: a.display, reason: a.reason,
});

export const errorPayload = (message: string) => ({ message });

export const gapDetectedPayload = (subpath: string, unknownParents: string[]) => ({ subpath, unknown_parents: unknownParents });

/** The CLI's task_notification shape for an SDK background task lost with its worker. */
export function lostTaskPayload(sessionId: string, uuid: string, task: { taskId: string; toolUseId: string }, summary: string) {
  return {
    type: "system", subtype: "task_notification", uuid, session_id: sessionId, task_id: task.taskId,
    status: "stopped", reason: "worker_restart", summary, output_file: "",
    ...(task.toolUseId ? { tool_use_id: task.toolUseId } : {}),
  };
}

/**
 * An SDK user message as the view sees it: the user's own input (no note) or
 * a message the worker wrote for the model (a note: synthetic, with its kind).
 */
export function userMessagePayload(sessionId: string, uuid: string, content: unknown, note: NoteKind | undefined, timestamp: string) {
  return {
    type: "user", session_id: sessionId, uuid, parent_tool_use_id: null,
    message: { role: "user", content },
    ...(note ? { isSynthetic: true, circus_note: note } : {}),
    timestamp,
  };
}

const interruptAnnotations = new Set(["[Request interrupted by user]", "[Request interrupted by user for tool use]"]);

/** The CLI's own annotation of an interrupted turn ("[Request interrupted by user]"). */
export function isInterruptAnnotation(message: Record<string, unknown>): boolean {
  if (message.type !== "user" || message.parent_tool_use_id) return false;
  const content = (message.message as { content?: unknown } | undefined)?.content;
  if (typeof content === "string") return interruptAnnotations.has(content);
  return Array.isArray(content) && content.length === 1 &&
    (content[0] as { type?: unknown }).type === "text" && interruptAnnotations.has((content[0] as { text?: unknown }).text as string);
}

/**
 * The view event an SDK message is forwarded as, or undefined for messages
 * the view does not show (system/init, status, lifecycle, ...). The payload
 * is the SDK message JSON unchanged, except that the CLI's annotation of a
 * user's interrupt is marked as the user_interrupt note the fake worker
 * publishes (not when the worker interrupted the turn itself).
 */
export function sdkEvent(
  message: Record<string, unknown>, options: { interruptNote?: boolean } = {},
): { kind: Kind; json: string; ephemeral: boolean } | undefined {
  switch (message.type) {
    case "assistant":
      return { kind: Kind.ASSISTANT, json: JSON.stringify(message), ephemeral: false };
    case "stream_event":
      return { kind: Kind.ASSISTANT, json: JSON.stringify(message), ephemeral: true };
    case "user": {
      const note = (options.interruptNote ?? true) && isInterruptAnnotation(message);
      const payload = note ? { ...message, isSynthetic: true, circus_note: "user_interrupt" } : message;
      return { kind: Kind.USER, json: JSON.stringify(payload), ephemeral: false };
    }
    case "result":
      return { kind: Kind.RESULT, json: JSON.stringify(message), ephemeral: false };
    case "system":
      switch (message.subtype) {
        case "permission_denied":
          return { kind: Kind.PERMISSION_DENIED, json: JSON.stringify(message), ephemeral: false };
        case "task_started":
          return { kind: Kind.BGTASK_STARTED, json: JSON.stringify(message), ephemeral: false };
        case "task_notification":
          return { kind: Kind.BGTASK_ENDED, json: JSON.stringify(message), ephemeral: false };
      }
  }
  return undefined;
}

export const MAX_BATCH_EVENTS = 128;
const MAX_BATCH_BYTES = 4 << 20;
/** A single event above this is replaced by an ERROR event. */
export const MAX_EVENT_BYTES = 8 << 20;
/** Above this many queued bytes, ephemeral events are dropped and the runner waits. */
export const HIGH_WATER_BYTES = 32 << 20;
const EVENT_OVERHEAD = 16;
export const FLUSH_INTERVAL_MS = 100;

interface Queued {
  event: PublishEvents_Event;
  bytes: number;
}

/**
 * EventPublisher batches a session's view events into PublishEvents
 * (at most 128 per batch, about every 100 ms), one batch in flight. A batch
 * is retried with identical content and batch_seq until it is acknowledged;
 * INVALID consumes its batch_seq (docs/wire.md). Errors other than a
 * rejection end the publisher and are reported through onFatal.
 */
export class EventPublisher {
  #queue: Queued[] = [];
  #bytes = 0;
  #seq = 0n;
  #timer?: ReturnType<typeof setTimeout>;
  #pumping?: Promise<void>;
  #failed?: unknown;
  #drained: Array<() => void> = [];
  #closed = false;

  constructor(
    readonly ops: SessionOps,
    readonly log: Logger,
    readonly onFatal: (error: unknown) => void,
  ) {}

  get queuedBytes(): number {
    return this.#bytes;
  }

  add(kind: Kind, payload: object | string, ephemeral = false): void {
    if (this.#closed || this.#failed !== undefined) return;
    if (ephemeral && this.#bytes >= HIGH_WATER_BYTES / 2) return;
    let json = typeof payload === "string" ? payload : JSON.stringify(payload);
    let bytes = Buffer.byteLength(json);
    if (bytes > MAX_EVENT_BYTES) {
      if (ephemeral) return;
      this.log.warn("session event too large; replaced by an error event", { session_id: this.ops.sessionId, bytes });
      json = JSON.stringify(errorPayload(`A ${Kind[kind]} event of ${bytes} bytes was too large to publish.`));
      kind = Kind.ERROR;
      bytes = Buffer.byteLength(json);
    }
    const event = create(PublishEvents_EventSchema, { kind, json, ephemeral });
    this.#queue.push({ event, bytes: bytes + EVENT_OVERHEAD });
    this.#bytes += bytes + EVENT_OVERHEAD;
    this.#timer ??= setTimeout(() => {
      this.#timer = undefined;
      this.#pump();
    }, FLUSH_INTERVAL_MS);
  }

  /** Resolves when every event added so far is acknowledged (or consumed as INVALID). */
  async flush(): Promise<void> {
    for (;;) {
      if (this.#failed !== undefined) throw this.#failed;
      if (this.#queue.length === 0 && !this.#pumping) return;
      if (this.#timer) {
        clearTimeout(this.#timer);
        this.#timer = undefined;
      }
      await this.#pump();
    }
  }

  /** Resolves once the queue is below the high-water mark. */
  async drained(): Promise<void> {
    while (this.#bytes >= HIGH_WATER_BYTES && this.#failed === undefined && !this.#closed) {
      const { promise, resolve } = Promise.withResolvers<void>();
      this.#drained.push(resolve);
      this.#pump();
      await promise;
    }
  }

  close(): void {
    this.#closed = true;
    if (this.#timer) clearTimeout(this.#timer);
    this.#timer = undefined;
    for (const wake of this.#drained.splice(0)) wake();
  }

  #pump(): Promise<void> {
    this.#pumping ??= this.#send().finally(() => {
      this.#pumping = undefined;
      if (this.#queue.length && !this.#timer && this.#failed === undefined && !this.#closed) {
        this.#timer = setTimeout(() => {
          this.#timer = undefined;
          this.#pump();
        }, FLUSH_INTERVAL_MS);
      }
    });
    return this.#pumping;
  }

  async #send(): Promise<void> {
    while (this.#queue.length && this.#failed === undefined) {
      let count = 0;
      let size = 0;
      while (count < this.#queue.length && count < MAX_BATCH_EVENTS) {
        const next = this.#queue[count].bytes;
        if (count > 0 && size + next > MAX_BATCH_BYTES) break;
        size += next;
        count++;
      }
      const batch = this.#queue.slice(0, count);
      const value = create(PublishEventsSchema, { batchSeq: this.#seq + 1n, events: batch.map((q) => q.event) });
      try {
        await this.ops.call({ case: "publishEvents", value });
      } catch (error) {
        if (!(error instanceof RejectedError)) {
          this.#failed = error;
          for (const wake of this.#drained.splice(0)) wake();
          this.onFatal(error);
          return;
        }
        this.log.warn("session event batch rejected and dropped", {
          session_id: this.ops.sessionId, batch_seq: Number(this.#seq + 1n), error: error.message.slice(0, 512),
        });
      }
      this.#seq++;
      this.#queue.splice(0, count);
      this.#bytes -= size;
      if (this.#bytes < HIGH_WATER_BYTES) for (const wake of this.#drained.splice(0)) wake();
    }
  }
}
