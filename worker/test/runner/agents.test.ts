import { ReleaseLease_Reason, ReportTurn_Phase } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, test } from "vitest";
import { AgentKind } from "@circus/proto/circus/v1/agent_pb";
import { Kind } from "../../src/runner/events.js";
import { CLAUDE_EXECUTABLE, Harness, waitFor } from "./harness.js";
import { fixture } from "./replay.js";

const ID1 = "00000000-0000-4000-8000-000000000001";
const text = () => fixture("text").steps;

describe("the session's agent", () => {
  test("Claude Code starts from the installed executable", async () => {
    const asked: number[] = [];
    const h = new Harness([text()], { deps: { agentExecutable: (agent) => { asked.push(agent); return CLAUDE_EXECUTABLE; } } });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    await waitFor(() => h.link.reports.some((r) => r.phase === ReportTurn_Phase.ENDED), "ENDED");
    expect(h.query.options.pathToClaudeCodeExecutable).toBe(CLAUDE_EXECUTABLE);
    expect(asked.length).toBeGreaterThan(0);
    expect(asked.every((agent) => agent === AgentKind.CLAUDE_CODE)).toBe(true);
    h.runner.shutdown();
    await h.done;
  });

  test("without Claude Code installed at this worker's version the lease is released with ERROR and nothing starts", async () => {
    const h = new Harness([text()], { deps: { agentExecutable: () => undefined } });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.replay.instances).toEqual([]);
    expect(h.link.durable().find((e) => e.kind === Kind.ERROR)?.payload).toMatchObject({ message: expect.stringContaining("not installed") });
    expect(h.log.lines.some((l) => l.level === "error" && l.message.includes("not installed"))).toBe(true);
  });

  test("a session for another agent is released with ERROR", async () => {
    const h = new Harness([text()], { lease: (lease) => { lease.config!.agent = AgentKind.UNSPECIFIED; } });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.replay.instances).toEqual([]);
  });

  test("an uninstall between CLI starts releases the session instead of starting another CLI", async () => {
    // Installed when the lease starts, gone when the first CLI would start.
    let asked = 0;
    const h = new Harness([text()], { deps: { agentExecutable: () => ++asked === 1 ? CLAUDE_EXECUTABLE : undefined } });
    h.link.addInput({ inputId: ID1, text: "hello" });
    await h.start();
    expect(await h.done).toBe("failed");
    expect(asked).toBe(2);
    expect(h.link.released).toEqual([ReleaseLease_Reason.ERROR]);
    expect(h.replay.instances).toEqual([]);
  });
});
