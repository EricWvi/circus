import { create } from "@bufbuild/protobuf";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { AgentKind, AgentStatusSchema, AgentStatus_State } from "@circus/proto/circus/v1/agent_pb";
import { LinkLostError, WorkerLink } from "../src/link/index.js";
import { hello, serve, workerOptions } from "./peer.js";

const status = (state: AgentStatus_State, installedVersion = "") => create(AgentStatusSchema, {
  agent: AgentKind.CLAUDE_CODE, state, installedVersion, targetVersion: "2.1.330",
});

describe("agents on the worker link", () => {
  let server: Awaited<ReturnType<typeof serve>>;
  let link: WorkerLink;
  let current = status(AgentStatus_State.NOT_INSTALLED);

  beforeEach(async () => {
    server = await serve();
    current = status(AgentStatus_State.NOT_INSTALLED);
    link = new WorkerLink({
      ...workerOptions(server.url),
      attach: { maxSessions: 64, sdkVersion: "", localOnly: true, agents: () => [current] },
    });
  });
  afterEach(async () => {
    await link.close();
    await server.close();
  });

  async function connect() {
    const peer = await server.peers.take();
    await hello(peer);
    peer.welcome();
    const attach = await peer.receive();
    const resume = await peer.receive();
    return { peer, attach, resume };
  }

  it("carries the agents' current states in every Attach and reports changes on the ready connection", async () => {
    const ready = Promise.withResolvers<void>();
    link.onReady(() => ready.resolve());
    await expect(link.reportAgents([current])).rejects.toBeInstanceOf(LinkLostError);
    link.start();
    const first = await connect();
    expect(first.attach.payload).toMatchObject({ case: "attach", value: {
      agents: [{ agent: AgentKind.CLAUDE_CODE, state: AgentStatus_State.NOT_INSTALLED, targetVersion: "2.1.330" }],
    } });
    first.peer.resume(first.resume, []);
    await ready.promise;

    current = status(AgentStatus_State.INSTALLING);
    const reported = link.reportAgents([current]);
    const report = await first.peer.receive();
    expect(report.payload).toMatchObject({ case: "reportAgents", value: { agents: [{ state: AgentStatus_State.INSTALLING }] } });
    first.peer.ok(report.requestId);
    await reported;

    const rejected = link.reportAgents([current]);
    const again = await first.peer.receive();
    first.peer.send({ case: "sessionReply", value: { result: { case: "rejected", value: { code: 1, message: "bad" } } } }, again.requestId);
    await expect(rejected).rejects.toThrow("ReportAgents rejected");

    // A new connection's Attach carries the state as it is then.
    current = status(AgentStatus_State.INSTALLED, "2.1.330");
    first.peer.drop();
    const second = await connect();
    expect(second.attach.payload).toMatchObject({ case: "attach", value: {
      agents: [{ state: AgentStatus_State.INSTALLED, installedVersion: "2.1.330" }],
    } });
    second.peer.resume(second.resume, []);
  });

  it("delivers AgentsWanted as a hint, the latest replacing one not yet consumed", async () => {
    const hints = link.hints()[Symbol.asyncIterator]();
    link.start();
    const { peer, resume } = await connect();
    for (const version of ["1.0.0", "2.1.330"]) {
      peer.send({ case: "workerEvent", value: { event: { case: "agentsWanted", value: {
        agents: [{ agent: AgentKind.CLAUDE_CODE, version }],
      } } } }, 1n);
    }
    peer.send({ case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "s" } } } }, 1n);
    // The pong proves the worker read the hints before readiness.
    const pong = new Promise((resolve) => peer.ws.once("pong", resolve));
    peer.ws.ping();
    await pong;
    peer.resume(resume, []);
    expect((await hints.next()).value).toMatchObject({ event: { case: "agentsWanted", value: { agents: [{ version: "2.1.330" }] } } });
    expect((await hints.next()).value).toMatchObject({ event: { case: "offer", value: { sessionId: "s" } } });
    await hints.return?.();
  });
});
