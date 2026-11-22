import { createHash } from "node:crypto";
import { TurnOutcome } from "@circus/proto/circus/v1/session_pb";
import { ReleaseLease_Reason, ReportTurn_Phase, WorkerEvent_Nudge_Reason as Reason } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { RetainedBudget } from "../../src/runner/store.js";
import { Harness, waitFor } from "./harness.js";
import { fixture, type Step } from "./replay.js";

const ID1 = "00000000-0000-4000-8000-000000000001";
const ID2 = "00000000-0000-4000-8000-000000000002";
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);
const text = () => fixture("text").steps;
const pick = (pred: (m: Record<string, unknown>) => boolean) =>
  structuredClone((text().find((s) => s.t === "msg" && pred(s.m)) as { m: Record<string, unknown> }).m);
const init = (): Step => ({ t: "msg", m: { ...pick((m) => m.type === "system" && m.subtype === "init"), uuid: crypto.randomUUID() } });
const assistant = (extra: Record<string, unknown> = {}): Step => ({ t: "msg", m: { ...pick((m) => m.type === "assistant"), uuid: crypto.randomUUID(), ...extra } });
const result = (extra: Record<string, unknown> = {}): Step => ({ t: "msg", m: { ...pick((m) => m.type === "result"), uuid: crypto.randomUUID(), ...extra } });
const started = (i: number): Step => ({ t: "msg", m: { type: "command_lifecycle", state: "started", command_uuid: `$PUSH${i}`, uuid: crypto.randomUUID() } });
const task = (id: string, backgrounded: boolean): Step => ({
  t: "msg", m: { type: "system", subtype: "task_started", task_id: id, tool_use_id: `toolu_${id}`, description: id, task_type: "local_bash", is_backgrounded: backgrounded, uuid: crypto.randomUUID(), session_id: "$SESSION" },
});

describe("budgets, bounds and restarts", () => {
  test("a session lost while an append hangs returns its retained budget, and a late append takes none", async () => {
    const retained = new RetainedBudget(64 << 20);
    const h = new Harness([fixture("text").steps], { deps: { retained } });
    h.link.interceptors.push((call, signal) => call.op.case === "appendTranscript"
      ? new Promise((_, reject) => signal?.addEventListener("abort", () => reject(signal.reason), { once: true })) : undefined);
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => h.link.calls.some((c) => c.op.case === "appendTranscript") && retained.used > 0, "append in flight");
    const store = h.query.store;
    h.runner.lose("revoked");
    expect(await h.done).toBe("lost");
    expect(retained.used).toBe(0);
    // The SDK's flush while the query closes.
    await expect(store.append({ projectKey: "", sessionId: h.link.claudeSessionId }, [{ type: "cost-state", pad: "x".repeat(1000) }])).rejects.toThrow();
    expect(retained.used).toBe(0);
  });

  test("ConsumeGrant in the hook gives up within its budget", async () => {
    const command = "rm -rf /tmp/x";
    const matchKey = createHash("sha256").update(command).digest("hex");
    const h = new Harness([[{ t: "push", i: 0 }, { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command }, tool_use_id: "toolu_1" } }]], {
      deps: { grantBudgetMs: 100 },
      lease: (lease) => { lease.state!.grants = [{ tool: "Bash", device: "box", matchKey } as never]; },
    });
    // The reply never comes: every attempt times out.
    h.link.interceptors.push((call, signal) => call.op.case === "consumeGrant"
      ? new Promise((_, reject) => signal?.addEventListener("abort", () => reject(signal.reason), { once: true })) : undefined);
    h.link.addInput({ inputId: ID1, text: "go" });
    await h.start();
    await waitFor(() => h.query?.hookResults.length === 1, "hook answered", 3_000);
    expect(h.query.hookResults[0]).toEqual({});
    h.runner.shutdown();
    await h.done;
  });

  test("foreground tasks are dropped with their turn, so background tasks stay tracked", async () => {
    const foreground = Array.from({ length: 260 }, (_, i) => task(`f${i}`, false));
    const script: Step[] = [
      { t: "push", i: 0 }, started(0), init(), ...foreground, assistant(), result({ user_message_uuids: ["$PUSH0"] }),
      { t: "push", i: 1 }, started(1), init(), task("bg", true), assistant(), result({ user_message_uuids: ["$PUSH1"] }),
    ];
    const h = new Harness([script], {
      replay: { onWaitPush: (i) => { if (i === 1) { h.link.addInput({ inputId: ID2, text: "two" }); h.runner.nudge([Reason.INPUT], 0n); } } },
    });
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    await waitFor(() => h.link.ops("patchState").some((p) => p.bgtasksStarted.some((t) => t.taskId === "bg")), "background task recorded");
    h.runner.shutdown();
    await h.done;
  });

  test("a failed authentication closes the query; the next input starts a new one", async () => {
    const script: Step[] = [
      { t: "push", i: 0 }, started(0), init(),
      assistant({ error: "authentication_failed" }),
      result({ is_error: true, terminal_reason: "api_error", user_message_uuids: ["$PUSH0"] }),
    ];
    const h = new Harness([script, text()]);
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "failed turn");
    expect(ended(h)[0]).toMatchObject({ outcome: TurnOutcome.ERROR, persisted: { inputIds: [ID1] } });
    await waitFor(() => h.replay.instances[0].closed, "query closed");
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "next turn on a new query");
    expect(h.replay.instances).toHaveLength(2);
    expect(h.replay.instances[1].pushed[0].uuid).toBe(ID2);
    h.runner.shutdown();
    await h.done;
  });

  test("an input no signal ever names stops keeping the session busy after a turn", async () => {
    const script: Step[] = [{ t: "push", i: 0 }, init(), assistant(), result({ user_message_uuid: undefined, user_message_uuids: [] })];
    const h = new Harness([script], { deps: { idleMs: 50 } });
    h.link.addInput({ inputId: ID1, text: "lost" });
    await h.start();
    expect(await h.done).toBe("released");
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
    expect(h.link.persistedInputs.has(ID1)).toBe(false);
    // One turn is not enough to say so: it may be racing a turn the CLI started.
    expect(h.log.lines.some((l) => l.message.includes("never attributed"))).toBe(false);
  });

  test("an input still unnamed after a second turn is logged", async () => {
    const script: Step[] = [
      { t: "push", i: 0 }, init(), assistant(), result({ user_message_uuid: undefined, user_message_uuids: [] }),
      init(), assistant(), result({ user_message_uuid: undefined, user_message_uuids: [] }),
    ];
    const h = new Harness([script]);
    h.link.addInput({ inputId: ID1, text: "lost" });
    await h.start();
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.log.lines.filter((l) => l.message.includes("never attributed"))).toHaveLength(1);
    h.runner.shutdown();
    await h.done;
  });

  test("after a failed login the query stays while a background task runs; the next input replaces it", async () => {
    const script: Step[] = [
      { t: "push", i: 0 }, started(0), init(), task("bg", true),
      assistant({ error: "authentication_failed" }),
      result({ is_error: true, terminal_reason: "api_error", user_message_uuids: ["$PUSH0"] }),
    ];
    const h = new Harness([script, text()]);
    h.link.addInput({ inputId: ID1, text: "one" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "failed turn");
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(h.replay.instances[0].closed).toBe(false);
    h.link.addInput({ inputId: ID2, text: "two" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "next turn on a new query");
    expect(h.replay.instances[0].closed).toBe(true);
    // The task went with the closed CLI: the model is told.
    const content = h.replay.instances[1].pushed[0].message.content as Array<{ text: string }>;
    expect(content[0].text).toContain("<task-id>bg</task-id>");
    h.runner.shutdown();
    await h.done;
  });
});
