import { SessionEvent_Kind as Kind, InputPriority, TurnOrigin, TurnOutcome } from "@circus/proto/circus/v1/session_pb";
import {
  ReleaseLease_Reason, ReportTurn_Phase, WorkerEvent_Nudge_Reason as Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { Harness, waitFor } from "./harness.js";
import { fixture, type Step } from "./replay.js";

const steps = (name: string) => fixture(name).steps;
const started = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.STARTED);
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);

async function oneInput(name: string, extra: Partial<ConstructorParameters<typeof Harness>[1]> = {}) {
  const h = new Harness([steps(name)], extra);
  h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "hello" });
  await h.start();
  return h;
}

describe("turns", () => {
  test("a pushed input starts a turn reported STARTED and ENDED with its persistence marks", async () => {
    const h = await oneInput("text");
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(started(h)).toMatchObject([{ localSeq: 1n, origin: TurnOrigin.INPUT }]);
    expect(ended(h)[0]).toMatchObject({
      localSeq: 1n, outcome: TurnOutcome.COMPLETED, transcriptIncomplete: false,
      persisted: { inputIds: ["00000000-0000-4000-8000-000000000001"], gapNoticeTurnNo: 0n },
    });
    expect(JSON.parse(ended(h)[0].usageJson)).toMatchObject({ num_turns: 1 });
    // The input went to the SDK with its id as the message uuid.
    expect(h.query.pushed).toMatchObject([{ type: "user", uuid: "00000000-0000-4000-8000-000000000001", message: { content: "hello" } }]);
    expect(h.query.pushed[0].priority).toBeUndefined();
    const kinds = h.link.durable().map((e) => e.kind);
    expect(kinds).toEqual([Kind.TURN_STARTED, Kind.USER, Kind.ASSISTANT, Kind.RESULT]);
    expect(h.link.durable()[0].payload).toEqual({ turn_no: "1", origin: "input" });
    expect(h.link.durable()[1].payload).toMatchObject({ uuid: "00000000-0000-4000-8000-000000000001", message: { role: "user", content: "hello" } });
    expect(h.link.events.some((e) => e.ephemeral && e.payload.type === "stream_event")).toBe(true);
    // The transcript reached the store through the adapter.
    expect(h.link.entries().some((e) => e.type === "assistant")).toBe(true);
    h.runner.shutdown();
    expect(await h.done).toBe("released");
  });

  test("a turn's events are acknowledged before its ENDED and before the next STARTED", async () => {
    const h = new Harness([steps("cli-turn")]);
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    const calls = h.link.calls;
    const endedAt = (seq: bigint) => calls.findIndex((c) => c.op.case === "reportTurn" && c.op.value.phase === ReportTurn_Phase.ENDED && c.op.value.localSeq === seq);
    const startedAt = (seq: bigint) => calls.findIndex((c) => c.op.case === "reportTurn" && c.op.value.phase === ReportTurn_Phase.STARTED && c.op.value.localSeq === seq);
    const resultBatch = (n: number) => h.link.events.filter((e) => e.kind === Kind.RESULT)[n].batchSeq;
    const publishAt = (seq: bigint) => calls.findIndex((c) => c.op.case === "publishEvents" && c.op.value.batchSeq === seq);
    expect(publishAt(resultBatch(0))).toBeLessThan(endedAt(1n));
    expect(endedAt(1n)).toBeLessThan(startedAt(2n));
    expect(publishAt(resultBatch(1))).toBeLessThan(endedAt(2n));
    // No batch of the first turn was sent after its ENDED.
    expect(calls.slice(endedAt(1n)).filter((c) => c.op.case === "publishEvents" && c.op.value.batchSeq <= resultBatch(0))).toEqual([]);
    h.runner.shutdown();
    await h.done;
  });

  test("a turn's start and its input are published before the CLI's next message is handled", async () => {
    const h = await oneInput("text");
    await waitFor(() => ended(h).length === 1, "ENDED");
    const startedEvent = h.link.events.find((e) => e.kind === Kind.TURN_STARTED)!;
    const user = h.link.events.find((e) => e.kind === Kind.USER)!;
    expect(user.batchSeq).toBe(startedEvent.batchSeq);
    // Nothing the CLI said after the turn's start shares that batch.
    expect(h.link.events.filter((e) => e.batchSeq === startedEvent.batchSeq).map((e) => e.kind)).toEqual([Kind.TURN_STARTED, Kind.USER]);
    h.runner.shutdown();
    await h.done;
  });

  test("a CLI-originated turn is reported with origin cli and its background task tracked", async () => {
    const h = new Harness([steps("cli-turn")]);
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(started(h).map((r) => r.origin)).toEqual([TurnOrigin.INPUT, TurnOrigin.CLI]);
    expect(ended(h)[1].persisted).toBeUndefined();
    expect(ended(h).map((r) => r.outcome)).toEqual([TurnOutcome.COMPLETED, TurnOutcome.COMPLETED]);
    const patches = h.link.ops("patchState");
    const task = patches.find((p) => p.bgtasksStarted.length)?.bgtasksStarted[0];
    expect(task).toMatchObject({ toolUseId: "toolu_rec_bg", kind: "local_bash" });
    expect(patches.some((p) => p.bgtasksEnded.includes(task!.taskId))).toBe(true);
    const kinds = h.link.durable().map((e) => e.kind);
    expect(kinds).toContain(Kind.BGTASK_STARTED);
    expect(kinds).toContain(Kind.BGTASK_ENDED);
    expect(h.link.durable().filter((e) => e.kind === Kind.TURN_STARTED).map((e) => e.payload.origin)).toEqual(["input", "cli"]);
    h.runner.shutdown();
    await h.done;
  });

  test("a NOW input stops the running turn, which ends STOPPED with an empty result", async () => {
    const h = new Harness([steps("now")], {
      replay: {
        onWaitPush: (i) => {
          if (i !== 1) return;
          h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000002", text: "right now", priority: InputPriority.NOW });
          h.runner.nudge([Reason.INPUT], 0n);
        },
      },
    });
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "first" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.query.pushed[1]).toMatchObject({ uuid: "00000000-0000-4000-8000-000000000002", priority: "now" });
    expect(ended(h)[0]).toMatchObject({ outcome: TurnOutcome.STOPPED, persisted: { inputIds: ["00000000-0000-4000-8000-000000000001"] } });
    expect(ended(h)[1]).toMatchObject({ outcome: TurnOutcome.COMPLETED, persisted: { inputIds: ["00000000-0000-4000-8000-000000000002"] } });
    expect(started(h).map((r) => r.origin)).toEqual([TurnOrigin.INPUT, TurnOrigin.INPUT]);
    // The replacement input is shown in the second turn, after its TURN_STARTED.
    const durable = h.link.durable();
    const second = durable.findIndex((e) => e.kind === Kind.TURN_STARTED && e.payload.turn_no === "2");
    expect(durable.findIndex((e) => e.kind === Kind.USER && e.payload.uuid === "00000000-0000-4000-8000-000000000002")).toBeGreaterThan(second);
    h.runner.shutdown();
    await h.done;
  });

  test("an input pushed during a turn merges into it and is marked with that turn", async () => {
    const h = new Harness([steps("merged")], {
      replay: {
        onWaitPush: (i) => {
          if (i !== 1) return;
          h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000002", text: "also this" });
          h.runner.nudge([Reason.INPUT], 0n);
        },
      },
    });
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "first" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(ended(h)[0].persisted?.inputIds).toEqual(["00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"]);
    expect(started(h)).toHaveLength(1);
    // The second TakeInputs returned the first input again (taken, not yet
    // persisted): it was not pushed a second time.
    expect(h.link.ops("takeInputs").length).toBeGreaterThanOrEqual(2);
    expect(h.query.pushed.map((m) => m.uuid)).toEqual(["00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"]);
    h.runner.shutdown();
    await h.done;
  });

  test("a result held back by a slow append is reported only after the append is acknowledged", async () => {
    let releaseAppend!: () => void;
    const held = new Promise<void>((resolve) => { releaseAppend = resolve; });
    const h = new Harness([steps("held")]);
    h.link.interceptors.push(async (call) => {
      if (call.op.case === "appendTranscript" && call.op.value.entries.some((e) => e.json.includes("HELD-REPLY"))) await held;
      return undefined;
    });
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "hello" });
    await h.start();
    await waitFor(() => h.link.durable().some((e) => e.kind === Kind.ASSISTANT), "assistant");
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(ended(h)).toHaveLength(0);
    expect(h.link.durable().some((e) => e.kind === Kind.RESULT)).toBe(false);
    releaseAppend();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(ended(h)[0].persisted?.inputIds).toEqual(["00000000-0000-4000-8000-000000000001"]);
    h.runner.shutdown();
    await h.done;
  });

  test("a CLI failure mid-turn ends the turn with ERROR, without marks, and releases with ERROR", async () => {
    const script: Step[] = steps("text").filter((s) => !(s.t === "msg" && s.m.type === "result"));
    const resultIndex = script.findIndex((s) => s.t === "msg" && s.m.type === "assistant");
    script.splice(resultIndex + 1, script.length, { t: "throw", message: "Claude Code process exited with code 1" });
    const f = new Harness([script]);
    f.link.addInput({ inputId: "00000000-0000-4000-8000-000000000001", text: "hello" });
    await f.start();
    expect(await f.done).toBe("failed");
    expect(ended(f)).toMatchObject([{ outcome: TurnOutcome.ERROR, transcriptIncomplete: false }]);
    expect(ended(f)[0].persisted).toBeUndefined();
    expect(f.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(f.link.durable().some((e) => e.kind === Kind.ERROR && String(e.payload.message).includes("exited with code 1"))).toBe(true);
  });
});

describe("attribution of inputs to turns", () => {
  const text = () => fixture("text").steps;
  const pick = (pred: (m: Record<string, unknown>) => boolean) =>
    structuredClone((text().find((s) => s.t === "msg" && pred(s.m)) as { m: Record<string, unknown> }).m);
  const init = () => ({ ...pick((m) => m.type === "system" && m.subtype === "init"), uuid: crypto.randomUUID() });
  const assistant = () => ({ ...pick((m) => m.type === "assistant"), uuid: crypto.randomUUID() });
  const result = (extra: Record<string, unknown>) => ({ ...pick((m) => m.type === "result"), uuid: crypto.randomUUID(), ...extra });
  const lifecycle = (state: string) => ({ type: "command_lifecycle", state, command_uuid: "$PUSH0", uuid: crypto.randomUUID() });
  const ID = "00000000-0000-4000-8000-000000000001";

  test("an input racing a turn the CLI started is not marked with that turn", async () => {
    const script: Step[] = [
      { t: "push", i: 0 },
      { t: "msg", m: lifecycle("queued") },
      // A background task's notification turn starts first.
      { t: "msg", m: init() },
      { t: "msg", m: assistant() },
      { t: "msg", m: result({ user_message_uuid: undefined, user_message_uuids: [], origin: { kind: "task-notification" } }) },
      // Then the input's own turn, which crashes before its result.
      { t: "msg", m: lifecycle("started") },
      { t: "msg", m: init() },
      { t: "throw", message: "Claude Code process exited with code 1" },
    ];
    const h = new Harness([script]);
    h.link.addInput({ inputId: ID, text: "hello" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(started(h).map((r) => r.origin)).toEqual([TurnOrigin.CLI, TurnOrigin.INPUT]);
    expect(ended(h).map((r) => [r.outcome, r.persisted?.inputIds ?? null])).toEqual([
      [TurnOutcome.COMPLETED, null], [TurnOutcome.ERROR, null],
    ]);
    // Not persisted: the control plane returns it to pending for the next epoch.
    expect(h.link.persistedInputs.has(ID)).toBe(false);
    // Named by its own turn right after the CLI's: no warning.
    expect(h.log.lines.some((l) => l.message.includes("never attributed"))).toBe(false);
    // Shown in the turn that took it.
    const durable = h.link.durable();
    const second = durable.findIndex((e) => e.kind === Kind.TURN_STARTED && e.payload.turn_no === "2");
    expect(durable.findIndex((e) => e.kind === Kind.USER && e.payload.uuid === ID)).toBeGreaterThan(second);
  });

  test("a result naming an input attributes it when no command_lifecycle announced it", async () => {
    const script: Step[] = [
      { t: "push", i: 0 },
      { t: "msg", m: init() },
      { t: "msg", m: assistant() },
      { t: "msg", m: result({ user_message_uuid: "$PUSH0", user_message_uuids: ["$PUSH0"] }) },
    ];
    const h = new Harness([script]);
    h.link.addInput({ inputId: ID, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(ended(h)[0].persisted?.inputIds).toEqual([ID]);
    h.runner.shutdown();
    await h.done;
  });

  test("an input the closed query never took is pushed again to the next query", async () => {
    const h = new Harness([steps("text"), steps("text")], { replay: { retryDelays: [10, 10] } });
    h.link.addInput({ inputId: ID, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "first turn");
    // A second input goes to the first query, whose script has ended: the CLI never takes it.
    h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000002", text: "second" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => h.query.pushed.length === 2, "second push");
    // A mirror_error between turns closes the query for the repair.
    h.query.emit({ type: "system", subtype: "mirror_error", error: "x", key: { projectKey: "p", sessionId: h.link.claudeSessionId }, uuid: crypto.randomUUID(), session_id: h.link.claudeSessionId });
    await waitFor(() => h.replay.instances.length === 2 && h.replay.instances[1].pushed.length === 1, "requeued push");
    expect(h.replay.instances[1].pushed[0].uuid).toBe("00000000-0000-4000-8000-000000000002");
    await waitFor(() => ended(h).length === 2, "second turn");
    expect(ended(h)[1].persisted?.inputIds).toEqual(["00000000-0000-4000-8000-000000000002"]);
    h.runner.shutdown();
    await h.done;
  });

  test("events between turns are acknowledged before the next STARTED", async () => {
    const h = new Harness([steps("cli-turn")]);
    h.link.addInput({ inputId: ID, text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    const endedBatch = h.link.events.find((e) => e.kind === Kind.BGTASK_ENDED)!.batchSeq;
    const publishAt = h.link.indexOf((c) => c.op.case === "publishEvents" && c.op.value.batchSeq === endedBatch);
    const startedAt = h.link.indexOf((c) => c.op.case === "reportTurn" && c.op.value.phase === ReportTurn_Phase.STARTED && c.op.value.localSeq === 2n);
    expect(publishAt).toBeGreaterThan(-1);
    expect(publishAt).toBeLessThan(startedAt);
    h.runner.shutdown();
    await h.done;
  });

  test("foreground tasks are not published as background tasks", async () => {
    const h = new Harness([steps("now")], {
      replay: { onWaitPush: (i) => { if (i === 1) { h.link.addInput({ inputId: "00000000-0000-4000-8000-000000000002", text: "now", priority: InputPriority.NOW }); h.runner.nudge([Reason.INPUT], 0n); } } },
    });
    h.link.addInput({ inputId: ID, text: "first" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.link.durable().filter((e) => e.kind === Kind.BGTASK_STARTED || e.kind === Kind.BGTASK_ENDED)).toEqual([]);
    expect(h.link.ops("patchState").filter((p) => p.bgtasksStarted.length)).toEqual([]);
    h.runner.shutdown();
    await h.done;
  });
});
