import { SessionEvent_Kind as Kind, TurnOutcome } from "@circus/proto/circus/v1/session_pb";
import {
  ReportTurn_Phase, SessionReply_Inputs_Input_Kind as InputKind, WorkerEvent_Nudge_Reason as Reason,
} from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { ledgerPrompt } from "../../src/runner/ledger.js";
import { Harness, waitFor } from "./harness.js";
import { fixture } from "./replay.js";

const text = () => fixture("text").steps;
const ended = (h: Harness) => h.link.reports.filter((r) => r.phase === ReportTurn_Phase.ENDED);
const NOTE = "00000000-0000-4000-8000-0000000000a1";
const LEDGER = "/home/worker/.local/state/circus/ledger/session-1";

describe("ledger snapshot in the system prompt", () => {
  test("the section states the directory and the decision file rules", () => {
    const prompt = ledgerPrompt(LEDGER);
    expect(prompt).toContain(`\`${LEDGER}\``);
    expect(prompt).toContain("not a git repository and not part of the working directory");
    expect(prompt).toContain("`<dir>/D<n>-<title>.md`");
    expect(prompt).toContain("the directory is its category");
    expect(prompt).toContain("exactly `number` (e.g. `D12`) and `title`");
    expect(prompt).toContain("Only decision files belong there");
    expect(prompt).toContain("`INDEX.md` lists them all: read it first");
    expect(prompt).toContain("Decisions are never deleted");
    expect(prompt).toContain("Moving a file changes its category");
    expect(prompt).toContain("Never change an existing number");
    expect(prompt).toContain("`D?a`, `D?b`, … (one to four lowercase letters)");
    expect(prompt).toContain("numbers are assigned when the changes are merged");
    expect(prompt).toContain("collected after each turn; problems are reported back to you as a note");
  });

  test("a lease with a ledger path appends the section to the preset", async () => {
    const h = new Harness([text()], { lease: (lease) => { lease.ledgerPath = LEDGER; } });
    h.link.addInput({ inputId: NOTE, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.query.options.systemPrompt).toEqual({ type: "preset", preset: "claude_code", append: ledgerPrompt(LEDGER) });
    h.runner.shutdown();
    await h.done;
  });

  test("a lease without one leaves the preset unchanged", async () => {
    const h = new Harness([text()]);
    h.link.addInput({ inputId: NOTE, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.query.options.systemPrompt).toEqual({ type: "preset", preset: "claude_code" });
    h.runner.shutdown();
    await h.done;
  });
});

describe("note inputs", () => {
  const body = "The ledger collection found problems:\n- core/D?a-x.md: front matter must hold number and title";

  test("a note starts a turn when idle, pushed synthetic and published with its category", async () => {
    const h = new Harness([text()]);
    await h.start();
    // Idle: the lease's first TakeInputs found nothing.
    await waitFor(() => h.link.ops("takeInputs").length >= 1, "first TakeInputs");
    expect(h.replay.instances).toHaveLength(0);

    h.link.addInput({ inputId: NOTE, text: body, kind: InputKind.NOTE, note: "ledger_problems" });
    h.runner.nudge([Reason.INPUT], 0n);
    await waitFor(() => ended(h).length === 1, "ENDED");

    expect(h.query.pushed).toHaveLength(1);
    expect(h.query.pushed[0]).toMatchObject({ uuid: NOTE, isSynthetic: true, message: { role: "user", content: body } });
    expect(h.query.pushed[0].priority).toBeUndefined();
    const durable = h.link.durable();
    expect(durable.find((e) => e.kind === Kind.TURN_STARTED)?.payload).toMatchObject({ origin: "input" });
    expect(durable.find((e) => e.kind === Kind.USER && e.payload.uuid === NOTE)?.payload)
      .toMatchObject({ isSynthetic: true, circus_note: "ledger_problems", message: { content: body } });
    expect(ended(h)).toMatchObject([{ outcome: TurnOutcome.COMPLETED, persisted: { inputIds: [NOTE] } }]);
    expect(h.link.persistedInputs.has(NOTE)).toBe(true);
    h.runner.shutdown();
    await h.done;
  });

  test("an unknown category passes through as given; an empty one publishes none", async () => {
    for (const [category, expected] of [["some_future_note", "some_future_note"], ["", undefined]] as const) {
      const h = new Harness([text()]);
      h.link.addInput({ inputId: NOTE, text: body, kind: InputKind.NOTE, note: category });
      await h.start();
      await waitFor(() => ended(h).length === 1, "ENDED");
      expect(h.query.pushed[0]).toMatchObject({ uuid: NOTE, isSynthetic: true });
      const event = h.link.durable().find((e) => e.kind === Kind.USER && e.payload.uuid === NOTE)!.payload;
      expect(event.isSynthetic).toBe(true);
      expect(event.circus_note).toBe(expected);
      h.runner.shutdown();
      await h.done;
    }
  });

  test("a user input is not synthetic", async () => {
    const h = new Harness([text()]);
    h.link.addInput({ inputId: NOTE, text: "hello" });
    await h.start();
    await waitFor(() => ended(h).length === 1, "ENDED");
    expect(h.query.pushed[0].isSynthetic).toBeUndefined();
    const event = h.link.durable().find((e) => e.kind === Kind.USER && e.payload.uuid === NOTE)!.payload;
    expect(event).not.toHaveProperty("isSynthetic");
    expect(event).not.toHaveProperty("circus_note");
    h.runner.shutdown();
    await h.done;
  });
});
