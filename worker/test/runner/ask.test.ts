import { createHash } from "node:crypto";
import { ApprovalKind, SessionEvent_Kind as Kind } from "@circus/proto/circus/v1/session_pb";
import {
  GetSessionState_Section, ReportTurn_Phase, SessionReply_Inputs_Input_Kind as InputKind, SessionReply_Rejected_Code as Code,
  WorkerEvent_Nudge_Reason as Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { DISALLOWED_TOOLS, QUESTION_INPUT_BYTES, denials } from "../../src/runner/approvals.js";
import { rejected } from "./fake-link.js";
import { Harness, waitFor } from "./harness.js";
import { fixture, type Step } from "./replay.js";

// canUseTool: the worker never waits for the user. A question becomes a
// QUESTION approval; any other call it is asked about becomes a TOOL
// approval, as a permission_denied does; both are denied at once.

const INPUT1 = "00000000-0000-4000-8000-000000000001";
const INPUT2 = "00000000-0000-4000-8000-000000000002";
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);
const command = "rm -rf /tmp/rec-denied";
const matchKey = createHash("sha256").update(command).digest("hex");

const questions = {
  questions: [
    { question: "Which color?", header: "Color", options: [{ label: "Red", description: "Warm" }, { label: "Blue", description: "Cool" }], multiSelect: false },
    { question: "Which features?", header: "Features", options: [{ label: "A", description: "a" }, { label: "B", description: "b" }], multiSelect: true },
  ],
};

/**
 * The denied fixture with the CLI asking canUseTool instead of reporting
 * permission_denied (as it does once canUseTool exists), after the hook;
 * denials replaces the result's permission_denials.
 */
function asked(ask: Extract<Step, { t: "canUse" }>, denialsOfResult: unknown[] = [], extra: Step[] = []): Step[] {
  const out: Step[] = [];
  for (const step of fixture("denied").steps) {
    if (step.t === "msg" && step.m.subtype === "permission_denied") continue;
    if (step.t === "msg" && step.m.type === "result") {
      out.push(...extra, { t: "msg", m: { ...step.m, permission_denials: denialsOfResult } });
      continue;
    }
    out.push(step);
    if (step.t === "hook") out.push(ask);
  }
  return out;
}

const askQuestion = (input: Record<string, unknown> = questions, agentID?: string): Extract<Step, { t: "canUse" }> =>
  ({ t: "canUse", tool: "AskUserQuestion", input, toolUseID: "toolu_ask", ...(agentID ? { agentID } : {}) });

async function run(steps: Step[], setup?: (h: Harness) => void): Promise<Harness> {
  const h = new Harness([steps]);
  h.link.addInput({ inputId: INPUT1, text: "go" });
  setup?.(h);
  await h.start();
  await waitFor(() => ended(h).length === 1, "the turn");
  return h;
}

/**
 * The worker publishes no APPROVAL_PENDING (the control plane records it
 * with the approval), and the call's tool_use is published before the
 * request, so the view shows the call before its card.
 */
function expectPublishedBeforeRequest(h: Harness, toolUseId: string): void {
  expect(h.link.durable().some((e) => e.kind === Kind.APPROVAL_PENDING)).toBe(false);
  const published = h.link.indexOf((c) => c.op.case === "publishEvents" &&
    (c.op.value as { events: Array<{ json: string }> }).events.some((e) => e.json.includes(`"id":"${toolUseId}"`)));
  const requested = h.link.indexOf((c) => c.op.case === "requestApproval");
  expect(published).toBeGreaterThanOrEqual(0);
  expect(requested).toBeGreaterThan(published);
}

async function finish(h: Harness): Promise<void> {
  h.runner.shutdown();
  await h.done;
}

describe("query options", () => {
  test("every query has canUseTool and disallows plan mode's tools", async () => {
    const h = await run(fixture("text").steps);
    expect(typeof h.query.options.canUseTool).toBe("function");
    expect(h.query.options.disallowedTools).toEqual(["EnterPlanMode", "ExitPlanMode"]);
    expect(DISALLOWED_TOOLS).toEqual(["EnterPlanMode", "ExitPlanMode"]);
    await finish(h);
  });
});

describe("AskUserQuestion", () => {
  test("on the main thread it becomes a question approval and the model is told to end its turn", async () => {
    // The result lists the denied question again: no second approval.
    const h = await run(asked(askQuestion(), [{ tool_name: "AskUserQuestion", tool_use_id: "toolu_ask", tool_input: questions }]));
    const inputJson = JSON.stringify(questions);
    expect(h.link.approvals).toEqual([expect.objectContaining({
      approvalId: "approval-1", kind: ApprovalKind.QUESTION, toolUseId: "toolu_ask", agentId: "", tool: "AskUserQuestion", device: "",
      matchKey: "", reason: "", display: "Which color?", inputJson,
    })]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.questionAsked }]);
    expect(h.query.canUseResults[0]).toMatchObject({ message: expect.stringContaining("shown to the user") });
    expectPublishedBeforeRequest(h, "toolu_rec_rm");
    await finish(h);
  });

  test("a long first question is clipped to 1024 bytes for the display", async () => {
    const long = { questions: [{ ...questions.questions[0], question: "é".repeat(600) }] };
    const h = await run(asked(askQuestion(long)));
    expect(Buffer.byteLength(h.link.approvals[0].display)).toBe(1024);
    expect(h.link.approvals[0].inputJson).toBe(JSON.stringify(long));
    await finish(h);
  });

  test("a subagent's question is denied without an approval", async () => {
    const h = await run(asked(askQuestion(questions, "agent-1"), [{ tool_name: "AskUserQuestion", tool_use_id: "toolu_ask", tool_input: questions }]));
    expect(h.link.approvals).toEqual([]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.subagentQuestion }]);
    expect(denials.subagentQuestion).toContain("Only the main conversation can ask the user questions");
    expect(h.link.durable().some((e) => e.kind === Kind.APPROVAL_PENDING)).toBe(false);
    await finish(h);
  });

  test("an input over 32 KiB is refused locally", async () => {
    const big = { questions: [{ ...questions.questions[0], question: "x".repeat(QUESTION_INPUT_BYTES) }] };
    const h = await run(asked(askQuestion(big)));
    expect(h.link.approvals).toEqual([]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.questionTooLarge }]);
    await finish(h);
  });

  test("a refused request is denied with an explanation and shown as an error", async () => {
    const h = await run(asked(askQuestion()), (h) => {
      h.link.interceptors.push((call) => call.op.case === "requestApproval" ? rejected(Code.INVALID, "input_json: question 1 has no header") : undefined);
    });
    // The model may ask again, corrected.
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.questionInvalid("input_json: question 1 has no header") }]);
    expect(h.query.canUseResults[0]).toMatchObject({ message: expect.stringMatching(/^Your questions were not shown to the user: input_json: question 1 has no header\. Ask again/) });
    expect(h.link.durable().some((e) => e.kind === Kind.ERROR && String(e.payload.message).includes("no header"))).toBe(true);
    expect(h.link.durable().some((e) => e.kind === Kind.APPROVAL_PENDING)).toBe(false);
    await finish(h);
  });

  test("the answers are pushed as an ordinary user message named question_answer, without a grant refresh", async () => {
    const text = 'The user answered your questions: "Which color?"="Blue". Continue with these answers in mind.';
    const second = fixture("text").steps.map((s) => (s.t === "push" ? { ...s, i: 1 } : s));
    const subst = JSON.parse(JSON.stringify(second).replaceAll("$PUSH0", "$PUSH1")) as Step[];
    const h = new Harness([[...asked(askQuestion()), ...subst]]);
    h.link.addInput({ inputId: INPUT1, text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "the first turn");
    const refreshes = h.link.ops("getState").length;
    h.link.addInput({ inputId: INPUT2, text, kind: InputKind.ANSWER });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 2, "the answered turn");
    expect(h.query.pushed[1]).toMatchObject({ uuid: INPUT2, message: { role: "user", content: text } });
    expect(h.query.pushed[1].isSynthetic).toBeUndefined();
    const event = h.link.durable().find((e) => e.kind === Kind.USER && e.payload.uuid === INPUT2)!.payload;
    expect(event).toMatchObject({ circus_note: "question_answer", message: { content: text } });
    expect(event.isSynthetic).toBeUndefined();
    expect(h.link.ops("getState").filter((op) => (op as { sections: number[] }).sections.includes(GetSessionState_Section.GRANTS)).length)
      .toBe(refreshes);
    await finish(h);
  });
});

describe("a call the permission mode asks about", () => {
  const askBash: Extract<Step, { t: "canUse" }> = {
    t: "canUse", tool: "Bash", input: { command, description: "Remove" }, toolUseID: "toolu_rec_rm", decisionReason: "This command needs approval",
  };

  test("becomes a tool approval, once, and the model is told not to retry it", async () => {
    // A permission_denied and the result's denials for the same call ask nothing more.
    const permissionDenied: Step = { t: "msg", m: { type: "system", subtype: "permission_denied", tool_name: "Bash", tool_use_id: "toolu_rec_rm",
      decision_reason_type: "mode", session_id: "$SESSION", uuid: crypto.randomUUID() } };
    const h = await run(asked(askBash, [{ tool_name: "Bash", tool_use_id: "toolu_rec_rm", tool_input: askBash.input }], [permissionDenied]));
    expect(h.link.approvals).toEqual([expect.objectContaining({
      approvalId: "approval-1", kind: ApprovalKind.UNSPECIFIED, toolUseId: "toolu_rec_rm", tool: "Bash", device: "box", matchKey,
      display: command, reason: "This command needs approval", inputJson: "",
    })]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolSent }]);
    expect(denials.toolSent).toContain("sent to the user for approval");
    expectPublishedBeforeRequest(h, "toolu_rec_rm");
    await finish(h);
  });

  test("a refused request tells the model the call was not allowed", async () => {
    const h = await run(asked(askBash), (h) => {
      h.link.interceptors.push((call) => call.op.case === "requestApproval" ? rejected(Code.LIMIT, "too many") : undefined);
    });
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolFailed }]);
    await finish(h);
  });

  const hook = (id: string): Step => ({ t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: askBash.input, tool_use_id: id } });
  const retry = (id: string): Extract<Step, { t: "canUse" }> => ({ ...askBash, toolUseID: id });

  /** The asked call, then the approval's retry turn running then. */
  async function approved(then: Step[]): Promise<Harness> {
    const h = new Harness([[...asked(askBash), { t: "push", i: 1 }, ...then]]);
    h.link.addInput({ inputId: INPUT1, text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "the first turn");
    h.link.grant("Bash", matchKey, "box");
    h.link.addInput({ inputId: INPUT2, text: "Retry it now.", kind: InputKind.APPROVAL });
    h.runner.nudge([Reason.APPROVAL], 0n);
    await waitFor(() => h.query.canUseResults.length === 1 + then.filter((s) => s.t === "canUse").length, "the retries");
    return h;
  }

  test("an approved retry an ask rule asks about after the hook allowed it is allowed; the next call is asked about", async () => {
    const h = await approved([hook("toolu_retry"), retry("toolu_retry"), hook("toolu_next"), retry("toolu_next")]);
    expect(h.query.hookResults.slice(1)).toEqual([
      expect.objectContaining({ hookSpecificOutput: expect.objectContaining({ permissionDecision: "allow" }) }), {},
    ]);
    expect(h.query.canUseResults.slice(1)).toEqual([
      { behavior: "allow", updatedInput: askBash.input },
      { behavior: "deny", message: denials.toolSent },
    ]);
    // One grant, consumed once, by the hook.
    expect(h.link.ops("consumeGrant").map((c) => (c as { toolUseId: string }).toolUseId)).toEqual(["toolu_retry"]);
    expect(h.link.approvals.map((a) => a.toolUseId)).toEqual(["toolu_rec_rm", "toolu_next"]);
    await finish(h);
  });

  test("an approved retry that reaches canUseTool without the hook consumes the grant there", async () => {
    const h = await approved([retry("toolu_retry")]);
    expect(h.query.canUseResults[1]).toEqual({ behavior: "allow", updatedInput: askBash.input });
    expect(h.link.ops("consumeGrant")).toMatchObject([{ tool: "Bash", device: "box", matchKey, toolUseId: "toolu_retry" }]);
    expect(h.link.approvals.map((a) => a.toolUseId)).toEqual(["toolu_rec_rm"]);
    await finish(h);
  });
});

describe("dontAsk mode", () => {
  // The CLI denies AskUserQuestion itself in dontAsk mode, before
  // canUseTool: a permission_denied (decision_reason_type "mode") and a
  // result denial. Neither becomes an approval.
  const hook: Step = { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "AskUserQuestion", tool_input: questions, tool_use_id: "toolu_ask" } };
  const denied: Step = { t: "msg", m: { type: "system", subtype: "permission_denied", tool_name: "AskUserQuestion", tool_use_id: "toolu_ask",
    decision_reason_type: "mode", decision_reason: "running in don't ask mode", session_id: "$SESSION", uuid: crypto.randomUUID() } };
  const resultDenial = { tool_name: "AskUserQuestion", tool_use_id: "toolu_ask", tool_input: questions };

  /** The denied fixture with the CLI's dontAsk handling of a question in place of its Bash denial. */
  function dontAsk(withPermissionDenied: boolean, denialsOfResult: unknown[]): Step[] {
    const out: Step[] = [];
    for (const step of fixture("denied").steps) {
      if (step.t === "msg" && step.m.subtype === "permission_denied") {
        if (withPermissionDenied) out.push(denied);
        continue;
      }
      if (step.t === "hook") {
        out.push(hook);
        continue;
      }
      out.push(step.t === "msg" && step.m.type === "result" ? { t: "msg", m: { ...step.m, permission_denials: denialsOfResult } } : step);
    }
    return out;
  }

  test("the permission_denied of a question asks nothing", async () => {
    const h = await run(dontAsk(true, []));
    expect(h.link.approvals).toEqual([]);
    expect(h.query.canUseResults).toEqual([]);
    await finish(h);
  });

  test("the result's denial of a question asks nothing", async () => {
    const h = await run(dontAsk(false, [resultDenial]));
    expect(h.link.approvals).toEqual([]);
    await finish(h);
  });

  test("both together ask nothing", async () => {
    const h = await run(dontAsk(true, [resultDenial]));
    expect(h.link.approvals).toEqual([]);
    await finish(h);
  });
});

describe("canUseTool while the session ends", () => {
  test("every call is denied and nothing is asked", async () => {
    const h = new Harness([fixture("interrupt-stream").steps], { deps: { shutdownGraceMs: 100 } });
    h.link.addInput({ inputId: INPUT1, text: "go" });
    await h.start();
    await waitFor(() => h.link.reports.some((r) => r.phase === ReportTurn_Phase.STARTED), "STARTED");
    const canUse = h.query.options.canUseTool!;
    const ask = (tool: string, input: Record<string, unknown>, id: string) =>
      canUse(tool, input, { signal: new AbortController().signal, toolUseID: id, requestId: id });
    h.link.stopped = true;
    h.runner.stop();
    expect(await ask("AskUserQuestion", questions, "toolu_q")).toEqual({ behavior: "deny", message: denials.ending });
    expect(await ask("Bash", { command }, "toolu_b")).toEqual({ behavior: "deny", message: denials.ending });
    expect(h.link.approvals).toEqual([]);
    expect(await h.done).toBe("released");
  });
});

describe("grants", () => {
  test("a call the hook allowed whose input changed before canUseTool is not allowed by that grant", async () => {
    const input = { command, description: "Remove" };
    const h = new Harness([[...asked({ t: "canUse", tool: "Bash", input, toolUseID: "toolu_rec_rm" }), { t: "push", i: 1 },
      { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: input, tool_use_id: "toolu_retry" } },
      // Another hook rewrote the command after the circus hook allowed it.
      { t: "canUse", tool: "Bash", input: { command: "rm -rf /", description: "Remove" }, toolUseID: "toolu_retry" }]]);
    h.link.addInput({ inputId: INPUT1, text: "go" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "the first turn");
    h.link.grant("Bash", matchKey, "box");
    h.link.addInput({ inputId: INPUT2, text: "Retry it now.", kind: InputKind.APPROVAL });
    h.runner.nudge([Reason.APPROVAL], 0n);
    await waitFor(() => h.query.canUseResults.length === 2, "the rewritten retry");
    expect(h.query.hookResults.at(-1)).toMatchObject({ hookSpecificOutput: { permissionDecision: "allow" } });
    expect(h.query.canUseResults[1]).toEqual({ behavior: "deny", message: denials.toolSent });
    expect(h.link.approvals.map((a) => [a.toolUseId, a.display])).toEqual([["toolu_rec_rm", command], ["toolu_retry", "rm -rf /"]]);
    await finish(h);
  });
});

describe("permission modes", () => {
  test("plan mode is refused: the session ends with an error", async () => {
    const h = new Harness([fixture("text").steps], { lease: (lease) => { lease.config!.permissionMode = "plan"; } });
    h.link.addInput({ inputId: INPUT1, text: "go" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.link.durable().some((e) => e.kind === Kind.ERROR && String(e.payload.message).includes('Unknown permission mode "plan"'))).toBe(true);
    expect(h.replay.instances).toEqual([]);
  });
});
