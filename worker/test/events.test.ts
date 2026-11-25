import { readFileSync, readdirSync } from "node:fs";
import { SessionEvent_Kind as Kind } from "@circus/proto/circus/v1/session_pb";
import { SessionReply_Rejected_Code as Code } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import {
  EventPublisher, MAX_BATCH_EVENTS, approvalPendingPayload, errorPayload, gapDetectedPayload, lostTaskPayload, sdkEvent,
  turnStartedPayload, userMessagePayload,
} from "../src/runner/events.js";
import { Semaphore, SessionOps } from "../src/runner/ops.js";
import { FakeLink, rejected } from "./runner/fake-link.js";
import { captureLogger, waitFor } from "./runner/harness.js";

// The Go fake worker's shapes, kept by its golden tests
// (internal/testutil/fakeworker/sdk_test.go): the payloads it writes itself
// and the SDK messages it imitates.
interface Golden {
  name?: string;
  kind: string;
  ephemeral: boolean;
  payload: Record<string, unknown>;
}
const golden = (file: string) => JSON.parse(readFileSync(new URL(`../../internal/testutil/fakeworker/testdata/${file}`, import.meta.url), "utf8")) as Golden[];
const kindName = (kind: Kind) => `KIND_${Kind[kind]}`;

describe("event payloads equal the Go fake worker's", () => {
  const SESSION = "018f0000-0000-7000-8000-000000000001";
  const INPUT = "018f0000-0000-7000-8000-000000000002";
  const EVENT = "018f0000-0000-7000-8000-000000000003";
  const STAMP = "2026-01-02T03:04:05.006Z";
  const interrupt = [{ type: "text", text: "[Request interrupted by user]" }];
  const built: Record<string, [Kind, object]> = {
    turn_started: [Kind.TURN_STARTED, turnStartedPayload(18446744073709551615n, "input")],
    turn_started_cli: [Kind.TURN_STARTED, turnStartedPayload(2n, "cli")],
    approval_pending: [Kind.APPROVAL_PENDING, approvalPendingPayload({
      approvalId: EVENT, tool: "Bash", device: "box", toolUseId: "toolu_fake", display: "echo hi", reason: "Scripted Bash approval required",
    })],
    error: [Kind.ERROR, errorPayload("scripted worker error")],
    gap_detected: [Kind.TRANSCRIPT_GAP_DETECTED, gapDetectedPayload("", [EVENT, INPUT])],
    lost_task: [Kind.BGTASK_ENDED, lostTaskPayload(SESSION, EVENT, { taskId: "task-1", toolUseId: "toolu_fake" }, "Background task task-1 was lost.")],
    lost_task_without_tool_use: [Kind.BGTASK_ENDED, lostTaskPayload(SESSION, EVENT, { taskId: "task-1", toolUseId: "" }, "Background task task-1 was lost.")],
    user_input: [Kind.USER, userMessagePayload(SESSION, INPUT, "hello", undefined, STAMP)],
    note_approval_retry: [Kind.USER, userMessagePayload(SESSION, INPUT, "Retry it now.", "approval_retry", STAMP)],
    note_user_interrupt: [Kind.USER, userMessagePayload(SESSION, EVENT, interrupt, "user_interrupt", STAMP)],
    note_previous_turn_interrupted: [Kind.USER, userMessagePayload(SESSION, EVENT, "The previous turn was interrupted.", "previous_turn_interrupted", STAMP)],
    note_lost_bgtask: [Kind.USER, userMessagePayload(SESSION, EVENT, "Background task task-1 was lost.", "lost_bgtask", STAMP)],
    note_history_missing: [Kind.USER, userMessagePayload(SESSION, EVENT, "Part of the conversation history is missing.", "history_missing", STAMP)],
    // A control-plane note (a KIND_NOTE input) keeps its category; a ledger
    // note is not synthetic.
    note_ledger_problems: [Kind.USER, userMessagePayload(SESSION, INPUT, "[circus ledger] The ledger collection found problems.", "ledger_problems", STAMP, false)],
  };

  test.each(golden("worker_events.json").map((g) => [g.name!, g] as const))("%s", (name, g) => {
    const [kind, payload] = built[name] ?? [];
    expect(payload, `no TypeScript builder for ${name}`).toBeDefined();
    expect(kindName(kind!)).toBe(g.kind);
    expect(JSON.parse(JSON.stringify(payload))).toEqual(g.payload);
  });

  test("every golden payload has a builder", () => {
    expect(Object.keys(built).sort()).toEqual(golden("worker_events.json").map((g) => g.name).sort());
  });

  test.each(golden("sdk_messages.json").map((g, i) => [i, g] as const))("forwarded SDK message %i", (_, g) => {
    // The SDK message as the CLI emits it: the note is the worker's addition.
    const { circus_note: _note, isSynthetic: _synthetic, ...sdk } = g.payload;
    const event = sdkEvent(g.payload.circus_note === "user_interrupt" ? sdk : g.payload);
    expect(event).toBeDefined();
    expect(kindName(event!.kind)).toBe(g.kind);
    expect(event!.ephemeral).toBe(g.ephemeral);
    expect(JSON.parse(event!.json)).toEqual(g.payload);
  });

  test("messages the view does not show are not forwarded", () => {
    for (const message of [{ type: "system", subtype: "init" }, { type: "system", subtype: "status" }, { type: "command_lifecycle" },
      { type: "system", subtype: "task_updated" }, { type: "tool_progress" }, { type: "system", subtype: "mirror_error" }]) {
      expect(sdkEvent(message)).toBeUndefined();
    }
    expect(sdkEvent({ type: "system", subtype: "task_started" })?.kind).toBe(Kind.BGTASK_STARTED);
    expect(sdkEvent({ type: "system", subtype: "task_notification" })?.kind).toBe(Kind.BGTASK_ENDED);
    expect(sdkEvent({ type: "system", subtype: "permission_denied" })?.kind).toBe(Kind.PERMISSION_DENIED);
    // A subagent's message with the same text is not the turn's interrupt annotation.
    const sub = sdkEvent({ type: "user", parent_tool_use_id: "toolu_x", message: { content: "[Request interrupted by user]" } });
    expect(JSON.parse(sub!.json).circus_note).toBeUndefined();
  });
});

describe("EventPublisher", () => {
  function setup() {
    const link = new FakeLink("/");
    link.epoch = 1n;
    const ops = new SessionOps(link, "sess-1", 1n, new AbortController().signal, async () => {}, new Semaphore(8));
    const errors: unknown[] = [];
    const publisher = new EventPublisher(ops, captureLogger(), (e) => errors.push(e));
    return { link, publisher, errors };
  }

  test("batches at most 128 events with consecutive batch_seq values", async () => {
    const { link, publisher } = setup();
    for (let i = 0; i < 300; i++) publisher.add(Kind.ASSISTANT, { i });
    await publisher.flush();
    const batches = link.ops("publishEvents");
    expect(batches.map((b) => b.events.length)).toEqual([MAX_BATCH_EVENTS, MAX_BATCH_EVENTS, 300 - 2 * MAX_BATCH_EVENTS]);
    expect(batches.map((b) => b.batchSeq)).toEqual([1n, 2n, 3n]);
    expect(link.events.map((e) => e.payload.i)).toEqual([...Array(300).keys()]);
  });

  test("sends queued events after about 100 ms without a flush", async () => {
    const { link, publisher } = setup();
    publisher.add(Kind.ERROR, errorPayload("x"));
    expect(link.ops("publishEvents")).toHaveLength(0);
    await waitFor(() => link.events.length === 1, "timed batch");
  });

  test("retries a batch with identical content and treats INVALID as consumed", async () => {
    const { link, publisher, errors } = setup();
    let calls = 0;
    link.interceptors.push((call) => {
      if (call.op.case !== "publishEvents") return undefined;
      calls++;
      return calls === 1 ? rejected(Code.STORE_UNAVAILABLE) : undefined;
    });
    publisher.add(Kind.TURN_STOPPED, {});
    await publisher.flush();
    publisher.add(Kind.ERROR, errorPayload("next"));
    await publisher.flush();
    const batches = link.ops("publishEvents");
    expect(batches.map((b) => b.batchSeq)).toEqual([1n, 1n, 2n]);
    expect(batches[0]).toBe(batches[1]);
    expect(link.events.map((e) => e.payload)).toEqual([{ message: "next" }]);
    expect(errors).toEqual([]);
  });

  test("replaces an oversized event by an error event", async () => {
    const { link, publisher } = setup();
    publisher.add(Kind.USER, { big: "x".repeat(9 << 20) });
    await publisher.flush();
    expect(link.events).toMatchObject([{ kind: Kind.ERROR }]);
  });
});

describe("the fake worker's SDK shapes are a subset of the real SDK's", () => {
  // Every SDK message recorded from the real SDK and CLI (test/fixtures/sdk).
  const dir = new URL("./fixtures/sdk/", import.meta.url);
  const real = readdirSync(dir).filter((f) => f.endsWith(".json")).flatMap((f) =>
    (JSON.parse(readFileSync(new URL(f, dir), "utf8")) as { steps: Array<{ t: string; m?: Record<string, unknown> }> }).steps
      .filter((s) => s.t === "msg").map((s) => s.m!));
  const shape = (m: Record<string, unknown>): string => {
    if (m.type === "stream_event") return `stream_event/${(m.event as { type: string }).type}`;
    if (m.type === "system") return `system/${String(m.subtype)}`;
    if (m.type === "user") {
      const content = (m.message as { content?: unknown }).content;
      const toolResult = Array.isArray(content) && content.some((b) => (b as { type?: string }).type === "tool_result");
      return toolResult ? "user/tool_result" : "user/text";
    }
    if (m.type === "result") return `result/${String(m.subtype)}`;
    return String(m.type);
  };
  const keys = (messages: Array<Record<string, unknown>>, pick: (m: Record<string, unknown>) => unknown) =>
    new Set(messages.flatMap((m) => {
      const v = pick(m);
      return v && typeof v === "object" && !Array.isArray(v) ? Object.keys(v) : [];
    }));
  // Fields the worker adds to the view event, and SDK fields documented optional that the recordings lack.
  const workerAdded = new Set(["circus_note", "isSynthetic"]);
  const optional: Record<string, string[]> = {
    // SDKTaskNotificationMessage.reason: "worker_restart" for a task orphaned with its worker.
    "system/task_notification": ["reason"],
    // SDKUserMessage.timestamp; the fake stamps every user message it records.
    "user/text": ["timestamp"], "user/tool_result": ["timestamp"],
    // On a thinking block's events and durable message (the recordings have no thinking block).
    "stream_event/content_block_start": ["thinking_display"], assistant: ["thinking_display", "thinking_duration_ms"],
  };
  const fake = [...golden("sdk_messages.json"), ...golden("worker_events.json").filter((g) => g.name?.startsWith("lost_task"))];

  test.each(fake.map((g, i) => [i, shape(g.payload), g] as const))("fake message %i (%s)", (_, kind, g) => {
    const same = real.filter((m) => shape(m) === kind);
    expect(same.length, `no recorded real ${kind}`).toBeGreaterThan(0);
    const allowed = new Set([...keys(same, (m) => m), ...workerAdded, ...(optional[kind] ?? [])]);
    expect(Object.keys(g.payload).filter((k) => !allowed.has(k))).toEqual([]);
    // An assistant message and a stream event carry the Messages API's own
    // objects (from a fake API in the recordings); a user message's is the SDK's.
    if (g.payload.type === "user") {
      const nested = keys(same, (m) => m.message);
      expect(Object.keys(g.payload.message as object).filter((k) => !nested.has(k)), "message keys").toEqual([]);
    }
  });
});
