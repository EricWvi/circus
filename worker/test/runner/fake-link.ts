import { create } from "@bufbuild/protobuf";
import { SessionConfigSchema } from "@circus/proto/circus/v1/session_pb";
import {
  RecoverySchema, ReportTurn_Phase, SessionReplySchema, SessionReply_Inputs_InputSchema, SessionReply_LeaseSchema,
  SessionReply_Rejected_Code as Code, SessionStateSchema, SessionState_GrantSchema, WorkerEventSchema,
  type PublishEvents_Event, type ReportTurn, type SessionReply, type SessionReply_Inputs_Input,
  type SessionReply_Lease, type TranscriptEntry, type WorkerEvent,
} from "@circus/proto/circus/v1/worker_link_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { ResumeOutcome, SessionOp } from "../../src/link/index.js";
import type { LinkPort } from "../../src/runner/ops.js";

export const reply = (result: MessageInitShape<typeof SessionReplySchema>["result"]): SessionReply =>
  create(SessionReplySchema, { result });
export const ok = (deduplicated = false) => reply({ case: "ok", value: { deduplicated } });
export const rejected = (code: Code, message = "") => reply({ case: "rejected", value: { code, message } });
export const fenced = (epoch: bigint) => reply({ case: "fenced", value: { epoch } });

export interface Call {
  sessionId: string;
  epoch: bigint;
  op: SessionOp;
}

export type Interceptor = (call: Call, signal?: AbortSignal) => SessionReply | undefined | Promise<SessionReply | undefined>;

/** An async queue of hints, the shape WorkerLink.hints() has. */
class Hints implements AsyncIterable<WorkerEvent> {
  #items: WorkerEvent[] = [];
  #wake?: () => void;
  #closed = false;

  push(event: WorkerEvent): void {
    this.#items.push(event);
    this.#wake?.();
  }

  close(): void {
    this.#closed = true;
    this.#wake?.();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<WorkerEvent> {
    for (;;) {
      while (this.#items.length) yield this.#items.shift()!;
      if (this.#closed) return;
      await new Promise<void>((resolve) => { this.#wake = resolve; });
    }
  }
}

export interface PublishedEvent {
  kind: number;
  payload: Record<string, unknown>;
  ephemeral: boolean;
  batchSeq: bigint;
}

/**
 * FakeLink is an in-memory control plane behind the LinkPort surface: it
 * keeps leases, transcripts (with batch_seq and uuid deduplication), events,
 * inputs, turns, approvals and grants as docs/wire.md describes, closely
 * enough for the runner's tests. Interceptors can replace any reply.
 */
export class FakeLink implements LinkPort {
  readonly calls: Call[] = [];
  readonly held = new Map<string, bigint>();
  readonly hintQueue = new Hints();
  readonly interceptors: Interceptor[] = [];
  #ready = new Set<(outcomes: ResumeOutcome[]) => void>();

  epoch = 0n;
  /** The Lease returned by AcquireLease, completed with the epoch. */
  lease: SessionReply_Lease;
  pending: SessionReply_Inputs_Input[] = [];
  taken: SessionReply_Inputs_Input[] = [];
  persistedInputs = new Set<string>();
  turnNo = 0n;
  turns = new Map<bigint, bigint>();
  reports: ReportTurn[] = [];
  events: PublishedEvent[] = [];
  #eventSeq = 0n;
  transcripts = new Map<string, { seq: bigint; epoch: bigint; entries: TranscriptEntry[]; uuids: Set<string> }>();
  #eventEpoch = 0n;
  approvals: Array<{ approvalId: string; tool: string; device: string; matchKey: string; toolUseId: string; display: string; reason: string }> = [];
  grants: Array<{ tool: string; device: string; matchKey: string }> = [];
  consumed = new Map<string, string>();
  released: number[] = [];
  repaired = 0;
  stopped = false;
  /** The GetLaunchEnv reply; local login unless a test sets a provider's. */
  launchEnv: () => SessionReply = () => reply({ case: "launchEnv", value: { localLogin: true } });

  constructor(cwd: string, readonly sessionId = "sess-1", readonly claudeSessionId = "00000000-0000-4000-8000-0000000000aa") {
    this.lease = create(SessionReply_LeaseSchema, {
      claudeSessionId, hasTranscript: false, state: create(SessionStateSchema), recovery: create(RecoverySchema),
      config: create(SessionConfigSchema, { local: true, defaultDevice: "box", cwds: [{ device: "box", cwd }] }),
    });
  }

  // ---- LinkPort ----

  async session(sessionId: string, epoch: bigint, op: SessionOp, signal?: AbortSignal): Promise<SessionReply> {
    signal?.throwIfAborted();
    const call = { sessionId, epoch, op };
    this.calls.push(call);
    for (const intercept of this.interceptors) {
      const result = await intercept(call, signal);
      if (result) return result;
    }
    return this.handle(call);
  }

  setHeld(sessionId: string, epoch: bigint | null): void {
    if (epoch === null) this.held.delete(sessionId);
    else this.held.set(sessionId, epoch);
  }

  onReady(listener: (outcomes: ResumeOutcome[]) => void): () => void {
    this.#ready.add(listener);
    return () => this.#ready.delete(listener);
  }

  hints(): AsyncIterable<WorkerEvent> {
    return this.hintQueue;
  }

  // ---- Test controls ----

  ready(outcomes: ResumeOutcome[] = []): void {
    for (const listener of this.#ready) listener(outcomes);
  }

  hint(event: MessageInitShape<typeof WorkerEventSchema>["event"]): void {
    this.hintQueue.push(create(WorkerEventSchema, { event }));
  }

  addInput(init: MessageInitShape<typeof SessionReply_Inputs_InputSchema>): SessionReply_Inputs_Input {
    const input = create(SessionReply_Inputs_InputSchema, init);
    this.pending.push(input);
    return input;
  }

  grant(tool: string, matchKey: string, device = ""): void {
    this.grants.push({ tool, device, matchKey });
  }

  ops<C extends SessionOp["case"]>(kind: C): Array<Extract<SessionOp, { case: C }>["value"]> {
    return this.calls.filter((c) => c.op.case === kind).map((c) => c.op.value as never);
  }

  /** Index of the first call matching pred, for ordering assertions. */
  indexOf(pred: (call: Call) => boolean): number {
    return this.calls.findIndex(pred);
  }

  entries(subpath = ""): Array<Record<string, unknown>> {
    return (this.transcripts.get(subpath)?.entries ?? []).map((e) => JSON.parse(e.json) as Record<string, unknown>);
  }

  durable(): PublishedEvent[] {
    return this.events.filter((e) => !e.ephemeral);
  }

  // ---- The control plane ----

  handle({ epoch, op }: Call): SessionReply {
    if (op.case !== "acquireLease" && epoch !== this.epoch) return fenced(this.epoch);
    switch (op.case) {
      case "acquireLease": {
        this.epoch++;
        const lease = create(SessionReply_LeaseSchema, { ...this.lease, epoch: this.epoch });
        return reply({ case: "lease", value: lease });
      }
      case "releaseLease":
        this.released.push(op.value.reason);
        return ok();
      case "appendTranscript": {
        const key = op.value.subpath;
        let t = this.transcripts.get(key);
        if (!t) this.transcripts.set(key, t = { seq: 0n, epoch, entries: [], uuids: new Set() });
        // batch_seq starts at 1 per (epoch, subpath).
        if (t.epoch !== epoch) {
          t.epoch = epoch;
          t.seq = 0n;
        }
        if (op.value.batchSeq <= t.seq) return ok(true);
        if (op.value.batchSeq !== t.seq + 1n) return rejected(Code.INVALID, "batch_seq gap");
        for (const entry of op.value.entries) {
          if (entry.uuid && t.uuids.has(entry.uuid)) continue;
          if (entry.uuid) t.uuids.add(entry.uuid);
          t.entries.push(entry);
        }
        t.seq = op.value.batchSeq;
        return ok();
      }
      case "loadTranscript": {
        const t = this.transcripts.get(op.value.subpath);
        return reply({ case: "transcriptPage", value: { entries: t?.entries ?? [], nextSeq: 0n, done: true } });
      }
      case "listTranscriptKeys":
        return reply({ case: "transcriptKeys", value: { subpaths: [...this.transcripts.keys()] } });
      case "takeInputs": {
        if (this.stopped) return rejected(Code.STOPPED);
        this.taken.push(...this.pending.splice(0));
        return reply({ case: "inputs", value: { inputs: this.taken.filter((i) => !this.persistedInputs.has(i.inputId)) } });
      }
      case "reportTurn": {
        const { localSeq, phase } = op.value;
        this.reports.push(op.value);
        if (phase === ReportTurn_Phase.STARTED) {
          if (!this.turns.has(localSeq)) this.turns.set(localSeq, ++this.turnNo);
        } else if (!this.turns.has(localSeq)) {
          return rejected(Code.INVALID, "ENDED without STARTED");
        }
        for (const id of op.value.persisted?.inputIds ?? []) this.persistedInputs.add(id);
        return reply({ case: "turn", value: { turnNo: this.turns.get(localSeq)! } });
      }
      case "patchState":
        for (const id of op.value.persisted?.inputIds ?? []) this.persistedInputs.add(id);
        return ok();
      case "getState":
        return reply({
          case: "state",
          value: { state: create(SessionStateSchema, { stopped: this.stopped, grants: this.grants.map((g) => create(SessionState_GrantSchema, g)) }) },
        });
      case "publishEvents": {
        if (this.#eventEpoch !== epoch) {
          this.#eventEpoch = epoch;
          this.#eventSeq = 0n;
        }
        if (op.value.batchSeq <= this.#eventSeq) return ok(true);
        if (op.value.batchSeq !== this.#eventSeq + 1n) return rejected(Code.INVALID, "batch_seq gap");
        this.#eventSeq = op.value.batchSeq;
        // Kinds only the control plane records: the batch is dropped and its batch_seq consumed.
        if (op.value.events.some((e) => [8, 9, 10, 15, 17, 18, 19].includes(e.kind))) return rejected(Code.INVALID, "control-plane kind");
        for (const e of op.value.events as PublishEvents_Event[]) {
          this.events.push({ kind: e.kind, payload: JSON.parse(e.json) as Record<string, unknown>, ephemeral: e.ephemeral, batchSeq: op.value.batchSeq });
        }
        return ok();
      }
      case "requestApproval": {
        if (this.stopped) return rejected(Code.STOPPED);
        const approvalId = `approval-${this.approvals.length + 1}`;
        this.approvals.push({ approvalId, ...op.value });
        return reply({ case: "approval", value: { approvalId, deduplicated: false } });
      }
      case "consumeGrant": {
        const { tool, device, matchKey, toolUseId } = op.value;
        const key = `${tool}\n${device}\n${matchKey}`;
        if (this.consumed.get(toolUseId) === key) return reply({ case: "grant", value: { allowed: true } });
        const index = this.grants.findIndex((g) => g.tool === tool && g.device === device && g.matchKey === matchKey);
        if (index < 0 || this.consumed.has(toolUseId)) return reply({ case: "grant", value: { allowed: false } });
        this.grants.splice(index, 1);
        this.consumed.set(toolUseId, key);
        return reply({ case: "grant", value: { allowed: true } });
      }
      case "transcriptRepaired":
        this.repaired++;
        return ok();
      case "getLaunchEnv":
        if (this.stopped) return rejected(Code.STOPPED);
        return this.launchEnv();
    }
    throw new Error(`unhandled op ${(op as SessionOp).case}`);
  }
}
