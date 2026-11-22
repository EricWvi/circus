import { createHash } from "node:crypto";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { create } from "@bufbuild/protobuf";
import { SessionEvent_Kind as Kind, TurnOutcome } from "@circus/proto/circus/v1/session_pb";
import {
  PatchSessionState_BackgroundTaskSchema, ReleaseLease_Reason, ReportTurn_Phase,
  SessionReply_Inputs_Input_Kind as InputKind, SessionReply_Rejected_Code as Code, WorkerEvent_Nudge_Reason as Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { fenced, rejected, reply } from "./fake-link.js";
import { Harness, waitFor } from "./harness.js";
import { fixture, type Step } from "./replay.js";

const steps = (name: string) => fixture(name).steps;
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);
const startedCount = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.STARTED).length;
const INPUT1 = "00000000-0000-4000-8000-000000000001";
const INPUT2 = "00000000-0000-4000-8000-000000000002";
const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));

function withInput(h: Harness, id = INPUT1, text = "hello"): Harness {
  h.link.addInput({ inputId: id, text });
  return h;
}

describe("interrupt", () => {
  test("an Interrupt at the running turn's number interrupts it; the turn ends STOPPED", async () => {
    const h = withInput(new Harness([steps("interrupt-stream")], {
      replay: { onWaitPush: (i) => { if (i === 1) { withInput(h, INPUT2, "follow"); h.runner.nudge([Reason.INPUT], 1n); } } },
    }));
    await h.start();
    await waitFor(() => startedCount(h) === 1, "STARTED");
    h.runner.nudge([Reason.INTERRUPT], 1n);
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.query.interrupts).toBe(1);
    expect(ended(h).map((r) => r.outcome)).toEqual([TurnOutcome.STOPPED, TurnOutcome.COMPLETED]);
    expect(ended(h)[0].persisted?.inputIds).toEqual([INPUT1]);
    // The CLI's annotation is shown as the user_interrupt note, and the aborted partial text as an assistant event.
    const durable = h.link.durable();
    expect(durable.find((e) => e.kind === Kind.USER && e.payload.circus_note === "user_interrupt")?.payload)
      .toMatchObject({ isSynthetic: true, message: { content: [{ type: "text", text: "[Request interrupted by user]" }] } });
    expect(durable.some((e) => e.kind === Kind.ASSISTANT && e.payload.aborted === true)).toBe(true);
    h.runner.shutdown();
    await h.done;
  });

  test("a tool interrupted by Interrupt also ends STOPPED", async () => {
    const h = withInput(new Harness([steps("interrupt-tool")], {
      replay: { onWaitPush: (i) => { if (i === 1) { withInput(h, INPUT2, "follow"); h.runner.nudge([Reason.INPUT], 1n); } } },
    }));
    await h.start();
    await waitFor(() => h.link.durable().some((e) => e.kind === Kind.ASSISTANT), "tool use");
    h.runner.nudge([Reason.INTERRUPT], 1n);
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(ended(h)[0].outcome).toBe(TurnOutcome.STOPPED);
    expect(h.link.durable().some((e) => e.payload.circus_note === "user_interrupt")).toBe(true);
    h.runner.shutdown();
    await h.done;
  });

  test("a stale interrupt level never interrupts a later turn", async () => {
    const h = withInput(new Harness([steps("cli-turn")]));
    await h.start();
    await waitFor(() => startedCount(h) === 2, "second turn");
    h.runner.nudge([Reason.INTERRUPT], 1n);
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.query.interrupts).toBe(0);
    expect(ended(h)[1].outcome).toBe(TurnOutcome.COMPLETED);
    h.runner.shutdown();
    await h.done;
  });

  test("an interrupt while idle changes nothing", async () => {
    const h = withInput(new Harness([steps("text")]));
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    h.runner.nudge([Reason.INTERRUPT], 5n);
    await sleep(50);
    expect(h.query.interrupts).toBe(0);
    h.runner.shutdown();
    await h.done;
  });
});

describe("interrupt timing", () => {
  test("an interrupt level arriving before the next turn's STARTED is answered is compared with its number", async () => {
    let releaseStarted!: () => void;
    const gate = new Promise<void>((resolve) => { releaseStarted = resolve; });
    const h = withInput(new Harness([steps("cli-turn")]));
    h.link.interceptors.push(async (call) => {
      if (call.op.case === "reportTurn" && call.op.value.phase === ReportTurn_Phase.STARTED && call.op.value.localSeq === 2n) await gate;
      return undefined;
    });
    await h.start();
    await waitFor(() => h.link.calls.some((c) => c.op.case === "reportTurn" && c.op.value.localSeq === 2n), "second STARTED sent");
    // A late hint for turn 1 while turn 2's number is still unknown.
    h.runner.nudge([Reason.INTERRUPT], 1n);
    await sleep(30);
    releaseStarted();
    await waitFor(() => ended(h).length === 2, "two turns");
    expect(h.query.interrupts).toBe(0);
    expect(ended(h)[1].outcome).toBe(TurnOutcome.COMPLETED);
    h.runner.shutdown();
    await h.done;
  });
});

describe("shutdown", () => {
  test("a running turn that ends within the grace keeps its outcome and marks, then the lease is released", async () => {
    const script = steps("text");
    const result = script.findIndex((s) => s.t === "msg" && s.m.type === "result");
    script.splice(result, 0, { t: "call", fn: async () => { await sleep(150); } });
    const h = withInput(new Harness([script]));
    await h.start();
    await waitFor(() => startedCount(h) === 1, "STARTED");
    h.runner.shutdown();
    expect(await h.done).toBe("released");
    expect(ended(h)).toMatchObject([{ outcome: TurnOutcome.COMPLETED, persisted: { inputIds: [INPUT1] } }]);
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
  });

  test("a turn still running after the grace is left to the release (INTERRUPTED)", async () => {
    const h = withInput(new Harness([steps("interrupt-stream")], { deps: { shutdownGraceMs: 100 } }));
    await h.start();
    await waitFor(() => startedCount(h) === 1, "STARTED");
    h.runner.shutdown();
    expect(await h.done).toBe("released");
    expect(ended(h)).toEqual([]);
    expect(h.query.interrupts).toBe(0);
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
  });
});

describe("stop, idle and lease loss", () => {
  test("STOP closes the query and releases with STOPPED, without ENDED", async () => {
    const h = withInput(new Harness([steps("interrupt-stream")]));
    await h.start();
    await waitFor(() => startedCount(h) === 1, "STARTED");
    h.runner.nudge([Reason.STOP], 0n);
    expect(await h.done).toBe("released");
    expect(h.query.closed).toBe(true);
    expect(h.processes.confirmed).toBeGreaterThan(0);
    // The CLI has exited before the lease is given back.
    expect(h.processes.confirmedAt[0]).toBeLessThan(h.link.indexOf((c) => c.op.case === "releaseLease"));
    expect(ended(h)).toEqual([]);
    expect(h.link.released).toEqual([ReleaseLease_Reason.STOPPED]);
  });

  test("a stopped lease is released at once with STOPPED", async () => {
    const h = withInput(new Harness([steps("text")], { lease: (lease) => { lease.state!.stopped = true; } }));
    await h.start();
    expect(await h.done).toBe("released");
    expect(h.link.released).toEqual([ReleaseLease_Reason.STOPPED]);
    expect(h.replay.instances).toHaveLength(0);
  });

  test("an idle session is released with IDLE after the idle time", async () => {
    const h = withInput(new Harness([steps("text")], { deps: { idleMs: 100 } }));
    await h.start();
    expect(await h.done).toBe("released");
    expect(ended(h)).toHaveLength(1);
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
    expect(h.query.closed).toBe(true);
  });

  test("a running background task holds the idle release up to its limit", async () => {
    const script = steps("cli-turn");
    const cut = script.findIndex((s) => s.t === "msg" && s.m.type === "system" && s.m.subtype === "task_updated");
    const h = withInput(new Harness([script.slice(0, cut)], { deps: { idleMs: 20, taskHoldMs: 600 } }));
    const begun = performance.now();
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    await sleep(200);
    expect(h.link.released).toEqual([]);
    expect(await h.done).toBe("released");
    expect(performance.now() - begun).toBeGreaterThanOrEqual(550);
    expect(h.log.lines.some((l) => l.message.includes("background tasks still running"))).toBe(true);
  });

  test("a lost lease closes the query without ENDED or release", async () => {
    const h = withInput(new Harness([steps("interrupt-stream")]));
    await h.start();
    await waitFor(() => startedCount(h) === 1, "STARTED");
    h.runner.lose("revoked");
    expect(await h.done).toBe("lost");
    expect(h.query.closed).toBe(true);
    expect(h.processes.confirmed).toBeGreaterThan(0);
    expect(ended(h)).toEqual([]);
    expect(h.link.ops("releaseLease")).toEqual([]);
  });

  test("FENCED on an append drops the lease", async () => {
    const h = withInput(new Harness([steps("text")]));
    h.link.interceptors.push((call) => call.op.case === "appendTranscript" ? fenced(9n) : undefined);
    await h.start();
    expect(await h.done).toBe("lost");
    expect(h.link.ops("releaseLease")).toEqual([]);
  });

  test("TRANSCRIPT_FULL on an append stops the session", async () => {
    const h = withInput(new Harness([steps("text")]));
    h.link.interceptors.push((call) => call.op.case === "appendTranscript" ? rejected(Code.TRANSCRIPT_FULL) : undefined);
    await h.start();
    expect(await h.done).toBe("released");
    expect(h.link.released).toEqual([ReleaseLease_Reason.STOPPED]);
  });

  test("a lease that is not local is released with ERROR and an error event", async () => {
    const h = withInput(new Harness([steps("text")], { lease: (lease) => { lease.config!.local = false; } }));
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.link.durable().map((e) => e.kind)).toEqual([Kind.ERROR]);
    expect(h.log.lines.some((l) => l.level === "error" && l.message.includes("not local"))).toBe(true);
    expect(h.replay.instances).toHaveLength(0);
  });

  test("a missing working directory is reported and the lease released with ERROR", async () => {
    const h = withInput(new Harness([steps("text")], { lease: (lease) => { lease.config!.cwds[0].cwd = "/nonexistent/circus"; } }));
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.link.durable()[0].payload.message).toContain("/nonexistent/circus");
  });
});

describe("query options", () => {
  test("local-mode SDK options", async () => {
    const h = withInput(new Harness([steps("text")], { lease: (lease) => { lease.config!.model = "claude-test"; } }));
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    const options = h.query.options;
    expect(options).toMatchObject({
      cwd: h.cwd, settingSources: ["user", "project", "local"], systemPrompt: { type: "preset", preset: "claude_code" },
      permissionMode: "auto", includePartialMessages: true, sessionId: h.link.claudeSessionId, sessionStoreFlush: "eager", model: "claude-test",
    });
    expect(options.resume).toBeUndefined();
    expect(options.tools).toBeUndefined();
    expect(options.mcpServers).toBeUndefined();
    expect(options.hooks?.PreToolUse).toHaveLength(1);
    expect(typeof options.spawnClaudeCodeProcess).toBe("function");
    h.runner.shutdown();
    await h.done;
  });
});

describe("recovery", () => {
  test("recovery waits for the next input and goes in front of it; the gap notice is acknowledged", async () => {
    const h = new Harness([steps("text")], {
      lease: (lease) => {
        lease.hasTranscript = true;
        lease.transcriptGapTurnNo = 3n;
        lease.recovery!.previousTurnInterrupted = true;
        lease.recovery!.lostBgtasks = [create(PatchSessionState_BackgroundTaskSchema, {
          taskId: "task-1", toolUseId: "toolu_old", kind: "local_bash", description: "build",
        })];
      },
    });
    await h.start();
    await sleep(100);
    // Recovery alone never runs a turn.
    expect(h.replay.instances).toHaveLength(0);
    withInput(h);
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.query.options.resume).toBe(h.link.claudeSessionId);
    const content = h.query.pushed[0].message.content as Array<{ type: string; text: string }>;
    expect(content).toHaveLength(4);
    expect(content.slice(0, 3).every((block) => block.text.startsWith("<system-reminder>\n"))).toBe(true);
    expect(content[0].text).toContain("history is missing");
    expect(content[1].text).toContain("previous turn was interrupted");
    expect(content[2].text).toContain("<tool-use-id>toolu_old</tool-use-id>");
    expect(content[2].text).toContain("<status>failed</status>");
    expect(content[3]).toEqual({ type: "text", text: "hello" });
    const durable = h.link.durable();
    expect(durable.slice(0, 6).map((e) => [e.kind, e.payload.circus_note ?? e.payload.reason ?? null])).toEqual([
      [Kind.TURN_STARTED, null], [Kind.USER, "history_missing"], [Kind.USER, "previous_turn_interrupted"],
      [Kind.USER, "lost_bgtask"], [Kind.BGTASK_ENDED, "worker_restart"], [Kind.USER, null],
    ]);
    expect(ended(h)[0].persisted).toMatchObject({ inputIds: [INPUT1], gapNoticeTurnNo: 3n });
    const patchAt = h.link.indexOf((c) => c.op.case === "patchState" && c.op.value.bgtasksEnded.includes("task-1"));
    const endedAt = h.link.indexOf((c) => c.op.case === "reportTurn" && c.op.value.phase === ReportTurn_Phase.ENDED);
    expect(patchAt).toBeGreaterThan(endedAt);
    h.runner.shutdown();
    await h.done;
  });

  test("a hole in a loaded transcript is published while the gap mark is set", async () => {
    const load = (q: import("./replay.js").ReplayQuery) => q.store.load({ projectKey: "p", sessionId: q.sessionId });
    const run = async (mark: bigint) => {
      const h = withInput(new Harness([[{ t: "call", fn: async (q) => { await load(q); } }, ...steps("text")]], {
        lease: (lease, link) => {
          lease.hasTranscript = true;
          lease.transcriptGapTurnNo = mark;
          link.transcripts.set("", { seq: 0n, epoch: 0n, uuids: new Set(), entries: [
            { uuid: "a", json: JSON.stringify({ type: "user", uuid: "a", parentUuid: null }) },
            { uuid: "c", json: JSON.stringify({ type: "user", uuid: "c", parentUuid: "lost" }) },
          ] as never });
        },
      }));
      await h.start();
      await waitFor(() => ended(h).length === 1, "ENDED");
      h.runner.shutdown();
      await h.done;
      return h.link.durable().filter((e) => e.kind === Kind.TRANSCRIPT_GAP_DETECTED).map((e) => e.payload);
    };
    expect(await run(2n)).toEqual([{ subpath: "", unknown_parents: ["lost"] }]);
    expect(await run(0n)).toEqual([]);
  });

  test("device calls in a local session's recovery are ignored and logged", async () => {
    const h = new Harness([], { lease: (lease) => { lease.recovery!.calls = [{ callId: "c1" } as never]; } });
    await h.start();
    await sleep(20);
    expect(h.log.lines.some((l) => l.level === "error" && l.message.includes("device calls"))).toBe(true);
    h.runner.shutdown();
    await h.done;
  });
});

describe("approvals", () => {
  const command = "rm -rf /tmp/rec-denied";
  const matchKey = createHash("sha256").update(command).digest("hex");

  /** The denied fixture, then the retry turn the approval input starts, whose Bash call the hook allows. */
  function approvalScript(): Step[] {
    const denied = steps("denied");
    const pick = (pred: (m: Record<string, unknown>) => boolean) =>
      structuredClone((denied.find((s) => s.t === "msg" && pred(s.m)) as { m: Record<string, unknown> }).m);
    const lifecycle = pick((m) => m.type === "command_lifecycle" && m.state === "started");
    lifecycle.command_uuid = "$PUSH1";
    const toolUse = pick((m) => m.type === "assistant" && JSON.stringify(m).includes("tool_use"));
    (toolUse.message as { content: Array<{ id: string }> }).content[0].id = "toolu_retry";
    toolUse.uuid = crypto.randomUUID();
    const result = pick((m) => m.type === "result");
    result.permission_denials = [];
    return [
      ...denied,
      { t: "push", i: 1 },
      { t: "msg", m: lifecycle },
      { t: "msg", m: pick((m) => m.type === "system" && m.subtype === "init") },
      { t: "msg", m: toolUse },
      { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command, description: "Remove" }, tool_use_id: "toolu_retry" } },
      { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command, description: "Remove" }, tool_use_id: "toolu_again" } },
      { t: "msg", m: { ...result, uuid: crypto.randomUUID() } },
    ];
  }

  test("a denial requests approval; the grant's retry is allowed once by the hook", async () => {
    const h = withInput(new Harness([approvalScript()]), INPUT1, "remove it");
    await h.start();
    await waitFor(() => ended(h).length === 1, "first turn");
    expect(h.link.approvals).toEqual([{
      approvalId: "approval-1", toolUseId: "toolu_rec_rm", agentId: "", tool: "Bash", device: "box", matchKey,
      display: command, reason: "fake classifier: destructive command",
    }].map((a) => expect.objectContaining(a)));
    const pending = h.link.durable().find((e) => e.kind === Kind.APPROVAL_PENDING)!.payload;
    expect(pending).toEqual({ approval_id: "approval-1", tool: "Bash", device: "box", tool_use_id: "toolu_rec_rm", display: command, reason: "fake classifier: destructive command" });
    expect(h.link.durable().some((e) => e.kind === Kind.PERMISSION_DENIED && e.payload.tool_use_id === "toolu_rec_rm")).toBe(true);
    // The first call found no grant.
    expect(h.query.hookResults[0]).toEqual({});
    expect(h.link.ops("consumeGrant")).toEqual([]);

    // The user approves: the control plane adds a grant and the approval input.
    h.link.grant("Bash", matchKey, "box");
    const text = `The user approved this Bash call on device "box" once: ${command}\nRetry it now.`;
    h.link.addInput({ inputId: INPUT2, text, kind: InputKind.APPROVAL });
    h.runner.nudge([Reason.APPROVAL], 0n);
    await waitFor(() => ended(h).length === 2, "retry turn");
    expect(h.query.pushed[1]).toMatchObject({ uuid: INPUT2, message: { content: text } });
    expect(h.query.pushed[1].priority).toBeUndefined();
    expect(h.query.hookResults[1]).toMatchObject({ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow" } });
    // One grant, one use: the next identical call is not allowed.
    expect(h.query.hookResults[2]).toEqual({});
    expect(h.link.ops("consumeGrant").map((c) => ({ ...c }))).toMatchObject([{ tool: "Bash", device: "box", matchKey, toolUseId: "toolu_retry" }]);
    const retry = h.link.durable().find((e) => e.kind === Kind.USER && e.payload.uuid === INPUT2)!.payload;
    expect(retry).toMatchObject({ circus_note: "approval_retry", isSynthetic: true, message: { content: text } });
    expect(ended(h)[1].persisted?.inputIds).toEqual([INPUT2]);
    h.runner.shutdown();
    await h.done;
  });
});

describe("mirror_error between turns", () => {
  test("a task notice the closed CLI never delivered goes to the model with the next input", async () => {
    // The background task ends while idle; the repair closes the CLI before its notification turn.
    const cli = steps("cli-turn");
    const cut = cli.findIndex((s) => s.t === "msg" && s.m.type === "system" && s.m.subtype === "background_tasks_changed" &&
      cli.slice(0, cli.indexOf(s)).some((p) => p.t === "msg" && p.m.subtype === "task_notification"));
    const h = withInput(new Harness([cli.slice(0, cut), steps("text")], { replay: { retryDelays: [10, 10] } }));
    await h.start();
    await waitFor(() => h.link.ops("patchState").some((p) => p.bgtasksEnded.length), "task ended");
    const taskId = h.link.ops("patchState").find((p) => p.bgtasksEnded.length)!.bgtasksEnded[0];
    h.query.emit({ type: "system", subtype: "mirror_error", error: "x", key: { projectKey: "p", sessionId: h.link.claudeSessionId }, uuid: crypto.randomUUID(), session_id: h.link.claudeSessionId });
    await waitFor(() => h.link.repaired === 1, "repaired");
    withInput(h, INPUT2, "again");
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "next turn");
    const content = h.replay.instances[1].pushed[0].message.content as Array<{ text: string }>;
    expect(content[0].text).toContain(`<task-id>${taskId}</task-id>`);
    expect(content[0].text).toContain("<status>completed</status>");
    h.runner.shutdown();
    await h.done;
  });
});

describe("approval edge cases", () => {
  const command = "rm -rf /tmp/rec-denied";
  const matchKey = createHash("sha256").update(command).digest("hex");
  const denied = () => steps("denied");

  test("result.permission_denials alone requests an approval", async () => {
    const script = denied().filter((s) => !(s.t === "msg" && s.m.subtype === "permission_denied"));
    const h = withInput(new Harness([script]));
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.link.approvals).toMatchObject([{ toolUseId: "toolu_rec_rm", tool: "Bash", device: "box", matchKey }]);
    h.runner.shutdown();
    await h.done;
  });

  test("a denial by an explicit rule or a hook is not asked about", async () => {
    for (const reason of ["rule", "hook"]) {
      const script = denied().map((s) => s.t === "msg" && s.m.subtype === "permission_denied"
        ? { ...s, m: { ...s.m, decision_reason_type: reason } } : s);
      const h = withInput(new Harness([script]));
      await h.start();
      await waitFor(() => ended(h).length === 1, "ENDED");
      expect(h.link.approvals).toEqual([]);
      h.runner.shutdown();
      await h.done;
    }
  });

  test("the hook allows only on ConsumeGrant allowed, and no grant is assumed before the user approves", async () => {
    const hook = (id: string): Step => ({ t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command }, tool_use_id: id } });
    // After the denial, an identical call before any approval: no ConsumeGrant at all.
    const h = withInput(new Harness([[...denied(), hook("toolu_early"), { t: "push", i: 1 }, hook("toolu_refused")]], {
      lease: (lease) => { lease.state!.grants = [{ tool: "Bash", device: "other-device", matchKey } as never]; },
    }));
    h.link.interceptors.push((call) => call.op.case === "consumeGrant" ? reply({ case: "grant", value: { allowed: false } }) : undefined);
    await h.start();
    await waitFor(() => h.query?.hookResults.length === 2, "early hook");
    expect(h.link.ops("consumeGrant")).toEqual([]);
    // The user approves, but the control plane refuses the grant at use.
    h.link.grant("Bash", matchKey, "box");
    h.link.addInput({ inputId: INPUT2, text: "retry", kind: InputKind.APPROVAL });
    h.runner.nudge([Reason.APPROVAL], 0n);
    await waitFor(() => h.query.hookResults.length === 3, "refused hook");
    expect(h.link.ops("getState").length).toBeGreaterThan(0);
    expect(h.link.ops("consumeGrant")).toMatchObject([{ toolUseId: "toolu_refused", device: "box" }]);
    expect(h.query.hookResults[2]).toEqual({});
    h.runner.shutdown();
    await h.done;
  });
});

describe("broken transcripts", () => {
  const isAssistantBatch = (call: { op: { case: string; value: unknown } }) =>
    call.op.case === "appendTranscript" &&
    (call.op.value as { entries: Array<{ json: string }> }).entries.some((e) => JSON.parse(e.json).type === "assistant");

  test("after mirror_error the repair resends exactly the unacknowledged batches; nothing is imported", async () => {
    // A stale transcript in the real config directory, which a resumed
    // query never wrote to: the repair must not use it.
    const config = mkdtempSync(join(tmpdir(), "circus-config-"));
    const previous = process.env.CLAUDE_CONFIG_DIR;
    process.env.CLAUDE_CONFIG_DIR = config;
    try {
      let down = true;
      // The CLI annotates the interrupt the worker sends after mirror_error.
      const annotation = (steps("interrupt-stream").find((s) => s.t === "msg" && s.m.type === "user") as { m: Record<string, unknown> }).m;
      const mirror = steps("mirror");
      mirror.splice(mirror.findIndex((s) => s.t === "msg" && s.m.type === "result"), 0, {
        t: "call", fn: async (q) => {
          await q.until(() => q.interrupts > 0, "the worker's interrupt");
          q.emit({ ...annotation, uuid: crypto.randomUUID() });
        },
      });
      const h = withInput(new Harness([mirror, steps("text")], {
        replay: { retryDelays: [10, 10] }, deps: { appendBudgetMs: 50 },
        lease: (lease) => { lease.hasTranscript = true; },
      }));
      const project = join(config, "projects", h.cwd.replace(/[^a-zA-Z0-9]/g, "-"));
      mkdirSync(project, { recursive: true });
      writeFileSync(join(project, `${h.link.claudeSessionId}.jsonl`), JSON.stringify({ type: "user", uuid: "stale-1", parentUuid: null }) + "\n");
      h.link.interceptors.push((call) => {
        if (call.op.case === "reportTurn" && call.op.value.phase === ReportTurn_Phase.ENDED) down = false;
        return down && isAssistantBatch(call) ? rejected(Code.STORE_UNAVAILABLE) : undefined;
      });
      await h.start();
      await waitFor(() => h.link.repaired === 1, "TranscriptRepaired");
      expect(ended(h)).toHaveLength(1);
      expect(ended(h)[0]).toMatchObject({ transcriptIncomplete: true });
      expect(ended(h)[0].persisted).toBeUndefined();
      expect(h.query.closed).toBe(true);
      expect(h.query.interrupts).toBe(1);
      // The batch the SDK gave up on went out again with its batch_seq and content, once acknowledged.
      const sent = h.link.ops("appendTranscript").filter((a) => a.entries.some((e) => JSON.parse(e.json).type === "assistant"));
      expect(new Set(sent.map((a) => a.batchSeq)).size).toBe(1);
      expect(new Set(sent.map((a) => a.entries.map((e) => e.json).join("\n"))).size).toBe(1);
      const stored = h.link.entries();
      expect(stored.filter((e) => e.type === "assistant")).toHaveLength(1);
      expect(stored.some((e) => e.uuid === "stale-1")).toBe(false);
      // Every entry the SDK appended is stored exactly once, uuid-less ones included.
      expect(h.link.durable().some((e) => e.kind === Kind.USER && JSON.stringify(e.payload).includes("[Request interrupted by user]"))).toBe(true);
      const appended = h.query.appended.flat();
      expect(stored).toEqual(appended);
      const patch = h.link.ops("patchState").find((p) => p.persisted);
      expect(patch?.persisted?.inputIds).toEqual([INPUT1]);
      const repairedAt = h.link.indexOf((c) => c.op.case === "transcriptRepaired");
      const patchAt = h.link.indexOf((c) => c.op.case === "patchState" && c.op.value.persisted !== undefined);
      expect(patchAt).toBeLessThan(repairedAt);
      expect(h.link.durable().some((e) => e.kind === Kind.ERROR && String(e.payload.message).includes("Saving the transcript failed"))).toBe(true);
      // The worker's own interrupt is not shown as the user's.
      expect(h.link.durable().some((e) => e.payload.circus_note === "user_interrupt")).toBe(false);
      // The session goes on with a new query that resumes the transcript.
      withInput(h, INPUT2, "again");
      h.runner.nudge([Reason.INPUT], 0n);
      await waitFor(() => ended(h).length === 2, "next turn");
      expect(h.replay.instances[1].options.resume).toBe(h.link.claudeSessionId);
      h.runner.shutdown();
      await h.done;
    } finally {
      if (previous === undefined) delete process.env.CLAUDE_CONFIG_DIR;
      else process.env.CLAUDE_CONFIG_DIR = previous;
    }
  });

  test("a batch refused as INVALID cannot be repaired: release with ERROR, the session stays BROKEN", async () => {
    const h = withInput(new Harness([steps("mirror")], { replay: { retryDelays: [10, 10] } }));
    h.link.interceptors.push((call) => isAssistantBatch(call) ? rejected(Code.INVALID, "bad batch") : undefined);
    await h.start();
    expect(await h.done).toBe("failed");
    expect(ended(h)[0]).toMatchObject({ transcriptIncomplete: true });
    expect(h.link.repaired).toBe(0);
    expect(h.link.ops("patchState").some((p) => p.persisted)).toBe(false);
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.link.durable().some((e) => String(e.payload.message ?? "").includes("Repairing the transcript failed"))).toBe(true);
  });

  test("a repair reports the closed CLI's background tasks lost and stops holding the lease for them", async () => {
    // The background task starts, then the next turn's mirror fails.
    const cli = steps("cli-turn");
    const cut = cli.findIndex((s) => s.t === "msg" && s.m.type === "system" && s.m.subtype === "task_updated");
    let down = false;
    const h = withInput(new Harness([cli.slice(0, cut), steps("text")], {
      replay: { retryDelays: [10, 10] }, deps: { appendBudgetMs: 50, idleMs: 100, taskHoldMs: 60_000 },
    }));
    await h.start();
    await waitFor(() => ended(h).length === 1, "first turn");
    const taskId = h.link.ops("patchState").find((p) => p.bgtasksStarted.length)!.bgtasksStarted[0].taskId;
    // A mirror_error outside a turn (an append after the result) triggers the repair.
    down = true;
    h.link.interceptors.push((call) => down && call.op.case === "appendTranscript" ? rejected(Code.STORE_UNAVAILABLE) : undefined);
    h.query.emit({ type: "system", subtype: "mirror_error", error: "outage", key: { projectKey: "p", sessionId: h.link.claudeSessionId }, uuid: crypto.randomUUID(), session_id: h.link.claudeSessionId });
    await new Promise((resolve) => setTimeout(resolve, 30));
    down = false;
    await waitFor(() => h.link.repaired === 1, "repaired");
    // The next input carries the notice; bgtasks_ended follows its turn.
    withInput(h, INPUT2, "again");
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "next turn");
    const content = h.replay.instances[1].pushed[0].message.content as Array<{ text: string }>;
    expect(content[0].text).toContain(`<task-id>${taskId}</task-id>`);
    expect(h.link.durable().some((e) => e.payload.circus_note === "lost_bgtask")).toBe(true);
    await waitFor(() => h.link.ops("patchState").some((p) => p.bgtasksEnded.includes(taskId)), "bgtasks_ended");
    // Nothing holds the lease any more: the idle release comes after idleMs, not taskHoldMs.
    expect(await h.done).toBe("released");
    expect(h.link.released).toEqual([ReleaseLease_Reason.IDLE]);
  });
});
