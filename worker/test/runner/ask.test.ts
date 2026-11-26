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
import { fixture, type ReplayQuery, type Step } from "./replay.js";

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

/** The denied fixture with its call's tool_use id replaced. */
const deniedWith = (toolUseId: string): Step[] =>
  JSON.parse(JSON.stringify(fixture("denied").steps).replaceAll("toolu_rec_rm", toolUseId)) as Step[];

/**
 * The denied fixture with the CLI asking canUseTool instead of reporting
 * permission_denied (as it does once canUseTool exists), after the hook,
 * about the call its assistant message carries; denials replaces the
 * result's permission_denials.
 */
function asked(ask: Extract<Step, { t: "canUse" }>, denialsOfResult: unknown[] = [], extra: Step[] = []): Step[] {
  const out: Step[] = [];
  for (const step of deniedWith(ask.toolUseID)) {
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

/** An assistant message carrying one Bash tool_use. */
function toolUseMessage(id: string, input: Record<string, unknown>): Step {
  return { t: "msg", m: {
    type: "assistant", parent_tool_use_id: null, session_id: "$SESSION", uuid: crypto.randomUUID(),
    message: { id: `msg_${id}`, type: "message", role: "assistant", model: "claude", content: [{ type: "tool_use", id, name: "Bash", input }] },
  } };
}

/** The result of the turn an approval's retry input starts. */
const retryResult: Step = (() => {
  const result = fixture("denied").steps.find((s) => s.t === "msg" && s.m.type === "result") as Extract<Step, { t: "msg" }>;
  return { t: "msg", m: JSON.parse(JSON.stringify({ ...result.m, permission_denials: [], uuid: crypto.randomUUID() }).replaceAll("$PUSH0", "$PUSH1")) };
})();

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
    expectPublishedBeforeRequest(h, "toolu_ask");
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

  // The CLI streams the call's assistant message, then runs the hook.
  const hook = (id: string): Step[] => [toolUseMessage(id, askBash.input),
    { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: askBash.input, tool_use_id: id } }];
  const retry = (id: string): Extract<Step, { t: "canUse" }> => ({ ...askBash, toolUseID: id });

  /** The asked call, then the approval's retry turn running then. */
  async function approved(then: Step[]): Promise<Harness> {
    const h = new Harness([[...asked(askBash), { t: "push", i: 1 }, ...then, retryResult]]);
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
    const h = await approved([...hook("toolu_retry"), retry("toolu_retry"), ...hook("toolu_next"), retry("toolu_next")]);
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

describe("an approval and its call's assistant message", () => {
  // The CLI writes the assistant message carrying a call before it asks
  // canUseTool about the call, but the SDK calls canUseTool as soon as it
  // reads the request, while the message may still wait in its queue for
  // the message loop. The approval is requested only once the message is
  // published.
  type Ask = { tool: string; input: Record<string, unknown>; toolUseID: string; agentID?: string; decisionReason?: string };
  const bash: Ask = { tool: "Bash", input: { command, description: "Remove" }, toolUseID: "toolu_rec_rm", decisionReason: "This command needs approval" };

  /**
   * Resolves once every pending microtask has run. With the fake link, a
   * call's path from canUseTool to RequestApproval has no timer or I/O: a
   * request made without waiting has reached the link by then.
   */
  const settled = () => new Promise<void>((resolve) => setImmediate(resolve));

  interface HeldOptions {
    signal?: AbortSignal;
    /** What happens while canUseTool runs with the message held; releases it by default. */
    meanwhile?: (q: ReplayQuery, h: Harness) => void | Promise<void>;
    /** Steps after the result of canUseTool, before the rest of the turn. */
    after?: Step[];
    /** The result's permission_denials. */
    denials?: unknown[];
  }

  /**
   * The denied fixture, its call replaced by ask's: the assistant message
   * carrying the call is emitted but held from the runner (as in the SDK's
   * queue) while canUseTool is called. heldAtRequest records, for each
   * approval request, whether the message was still held.
   */
  function held(ask: Ask, opts: HeldOptions = {}) {
    const heldAtRequest: boolean[] = [];
    let harness: Harness | undefined;
    const steps: Step[] = [];
    for (const step of deniedWith(ask.toolUseID)) {
      if (step.t === "hook" || (step.t === "msg" && step.m.subtype === "permission_denied")) continue;
      if (step.t === "msg" && step.m.type === "result") {
        steps.push({ t: "msg", m: { ...step.m, permission_denials: opts.denials ?? [] } });
        continue;
      }
      if (step.t !== "msg" || step.m.type !== "assistant" || !JSON.stringify(step.m).includes(`"id":"${ask.toolUseID}"`)) {
        steps.push(step);
        continue;
      }
      const message = step.m;
      steps.push({ t: "call", fn: async (q) => {
        q.hold();
        q.emit(JSON.parse(JSON.stringify(message).replaceAll("$SESSION", q.sessionId)) as Record<string, unknown>);
        const result = q.options.canUseTool!(ask.tool, ask.input, {
          signal: opts.signal ?? new AbortController().signal, toolUseID: ask.toolUseID, requestId: crypto.randomUUID(),
          ...(ask.agentID ? { agentID: ask.agentID } : {}), ...(ask.decisionReason ? { decisionReason: ask.decisionReason } : {}),
        });
        await settled();
        await (opts.meanwhile ?? ((q) => q.release()))(q, harness!);
        q.canUseResults.push((await result)!);
        q.release();
      } }, ...(opts.after ?? []));
    }
    const setup = (h: Harness) => {
      harness = h;
      h.link.interceptors.push((call) => {
        if (call.op.case === "requestApproval") heldAtRequest.push(h.query.isHeld);
        return undefined;
      });
    };
    return { steps, heldAtRequest, setup };
  }

  async function runHeld(ask: Ask, opts?: HeldOptions) {
    const { steps, heldAtRequest, setup } = held(ask, opts);
    const h = await run(steps, setup);
    return { h, heldAtRequest };
  }

  test("a question is asked once its call's message is published", async () => {
    const { h, heldAtRequest } = await runHeld({ tool: "AskUserQuestion", input: questions, toolUseID: "toolu_ask" });
    expect(heldAtRequest).toEqual([false]);
    expect(h.link.approvals).toEqual([expect.objectContaining({ kind: ApprovalKind.QUESTION, toolUseId: "toolu_ask" })]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.questionAsked }]);
    expectPublishedBeforeRequest(h, "toolu_ask");
    await finish(h);
  });

  test("a call the permission mode asks about is asked once its message is published", async () => {
    const { h, heldAtRequest } = await runHeld(bash);
    expect(heldAtRequest).toEqual([false]);
    expect(h.link.approvals).toEqual([expect.objectContaining({ kind: ApprovalKind.UNSPECIFIED, toolUseId: "toolu_rec_rm", display: command })]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolSent }]);
    expectPublishedBeforeRequest(h, "toolu_rec_rm");
    await finish(h);
  });

  test("a call interrupted before its message is published asks nothing, then or later", async () => {
    // The CLI then reports the denial again: it was handled.
    const abort = new AbortController();
    const permissionDenied: Step = { t: "msg", m: { type: "system", subtype: "permission_denied", tool_name: "Bash", tool_use_id: "toolu_rec_rm",
      decision_reason_type: "mode", session_id: "$SESSION", uuid: crypto.randomUUID() } };
    const { h } = await runHeld(bash, {
      signal: abort.signal, meanwhile: () => abort.abort(), after: [permissionDenied],
      denials: [{ tool_name: "Bash", tool_use_id: "toolu_rec_rm", tool_input: bash.input }],
    });
    expect(h.link.ops("requestApproval")).toEqual([]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolFailed }]);
    await finish(h);
  });

  test("a call whose message does not come in time is asked about anyway, with a warning", async () => {
    const { steps, heldAtRequest, setup } = held(bash, {
      meanwhile: async (_q, h) => waitFor(() => h.link.approvals.length === 1, "the request"),
    });
    const h = new Harness([steps], { deps: { publishWaitMs: 1 } });
    h.link.addInput({ inputId: INPUT1, text: "go" });
    setup(h);
    await h.start();
    await waitFor(() => ended(h).length === 1, "the turn");
    expect(heldAtRequest).toEqual([true]);
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolSent }]);
    expect(h.log.lines.some((l) => l.level === "warn" && l.message.includes("not published in time"))).toBe(true);
    await finish(h);
  });

  test("a call waiting when the query ends asks nothing", async () => {
    const { steps, setup } = held(bash, { meanwhile: (q) => q.close() });
    const h = new Harness([steps]);
    h.link.addInput({ inputId: INPUT1, text: "go" });
    setup(h);
    await h.start();
    await h.done;
    expect(h.query.canUseResults).toEqual([{ behavior: "deny", message: denials.toolFailed }]);
    expect(h.link.ops("requestApproval")).toEqual([]);
  });

  test("a subagent's call does not wait: its messages may never reach the SDK", async () => {
    const { h, heldAtRequest } = await runHeld({ ...bash, toolUseID: "toolu_sub", agentID: "agent-1" });
    expect(heldAtRequest).toEqual([true]);
    expect(h.link.approvals).toEqual([expect.objectContaining({ toolUseId: "toolu_sub", agentId: "agent-1" })]);
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
      toolUseMessage("toolu_retry", input),
      { t: "hook", input: { hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: input, tool_use_id: "toolu_retry" } },
      // Another hook rewrote the command after the circus hook allowed it.
      { t: "canUse", tool: "Bash", input: { command: "rm -rf /", description: "Remove" }, toolUseID: "toolu_retry" }, retryResult]]);
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
