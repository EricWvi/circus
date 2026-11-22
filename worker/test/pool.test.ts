import { create } from "@bufbuild/protobuf";
import {
  ResumeLeases_HeldSchema, SessionReply_Rejected_Code as Code, SessionReply_Resumed_OutcomeSchema, WorkerEventSchema,
  WorkerEvent_Nudge_Reason as Reason, type SessionReply_Lease,
} from "@circus/proto/circus/v1/worker_link_pb";
import type { MessageInitShape } from "@bufbuild/protobuf";
import { describe, expect, test } from "vitest";
import { SessionPool, type Runner } from "../src/pool.js";
import type { RunnerEnd } from "../src/runner/runner.js";
import { FakeLink, rejected } from "./runner/fake-link.js";
import { FakeProcesses, captureLogger, waitFor } from "./runner/harness.js";

class StubRunner implements Runner {
  ending = false;
  releasing = false;
  nudges: Array<[Reason[], bigint]> = [];
  lost?: string;
  stopped = false;
  shutdowns = 0;
  readonly #end = Promise.withResolvers<RunnerEnd>();
  constructor(readonly lease: SessionReply_Lease, readonly sessionId: string) {}
  get epoch() { return this.lease.epoch; }
  start() { return this.#end.promise; }
  nudge(reasons: Reason[], level: bigint) { this.nudges.push([reasons, level]); }
  stop() { this.stopped = true; }
  lose(reason: string) { this.lost = reason; this.#end.resolve("lost"); }
  /** When set, shutdown() starts a release that takes this long. */
  releaseMs?: number;
  shutdown() {
    this.shutdowns++;
    if (this.releaseMs === undefined) {
      this.#end.resolve("released");
      return;
    }
    this.ending = true;
    this.releasing = true;
    setTimeout(() => this.#end.resolve("released"), this.releaseMs);
  }
  end(how: RunnerEnd) { this.#end.resolve(how); }
}

function setup(maxSessions = 4) {
  const link = new FakeLink("/");
  const runners: StubRunner[] = [];
  const processes = new FakeProcesses();
  const log = captureLogger();
  const pool = new SessionPool({
    link, maxSessions, log, processes, runner: { query: () => { throw new Error("unused"); } },
    createRunner: (lease, id) => {
      const runner = new StubRunner(lease, id);
      runners.push(runner);
      return runner;
    },
  });
  const hint = (event: MessageInitShape<typeof WorkerEventSchema>["event"]) => pool.hint(create(WorkerEventSchema, { event }));
  const offer = (sessionId: string) => hint({ case: "offer", value: { sessionId } });
  const acquires = () => link.calls.filter((c) => c.op.case === "acquireLease");
  return { link, pool, runners, processes, hint, offer, acquires, log };
}

describe("SessionPool", () => {
  test("an Offer is acquired fresh, once, ignoring hints while the acquisition is in flight", async () => {
    const { link, runners, hint, offer, acquires } = setup();
    let release!: () => void;
    const gate = new Promise<void>((resolve) => { release = resolve; });
    link.interceptors.push(async (call) => { if (call.op.case === "acquireLease") await gate; return undefined; });
    offer("s1");
    hint({ case: "nudge", value: { sessionId: "s1", reasons: [Reason.INPUT] } });
    offer("s1");
    release();
    await waitFor(() => runners.length === 1, "runner");
    expect(acquires()).toHaveLength(1);
    expect(acquires()[0]).toMatchObject({ sessionId: "s1", epoch: 0n, op: { value: { fresh: true } } });
    expect(link.held.get("s1")).toBe(runners[0].epoch);
    expect(runners[0].nudges).toEqual([]);
  });

  test("a Nudge for a session not held acquires it fresh; for a held one it goes to its runner", async () => {
    const { runners, hint, acquires } = setup();
    hint({ case: "nudge", value: { sessionId: "s1", reasons: [Reason.INPUT], interruptTurnNo: 0n } });
    await waitFor(() => runners.length === 1, "runner");
    hint({ case: "nudge", value: { sessionId: "s1", reasons: [Reason.INTERRUPT], interruptTurnNo: 4n } });
    expect(runners[0].nudges).toEqual([[[Reason.INTERRUPT], 4n]]);
    expect(acquires()).toHaveLength(1);
  });

  test("holds at most max_sessions and admits a waiting Offer when one ends", async () => {
    const { link, runners, offer, processes } = setup(2);
    offer("a");
    offer("b");
    offer("c");
    await waitFor(() => runners.length === 2, "two runners");
    expect(runners.map((r) => r.sessionId)).toEqual(["a", "b"]);
    runners[0].end("released");
    await waitFor(() => runners.length === 3, "third runner");
    expect(runners[2].sessionId).toBe("c");
    expect(link.held.has("a")).toBe(false);
    expect(processes.cleaned).toEqual(["a"]);
  });

  test("a refused acquisition holds nothing", async () => {
    const { link, pool, runners, offer } = setup();
    link.interceptors.push((call) => call.op.case === "acquireLease" ? rejected(Code.NOT_OFFERED) : undefined);
    offer("s1");
    await waitFor(() => link.calls.length === 1, "acquire");
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(runners).toEqual([]);
    expect(pool.held).toBe(0);
  });

  test("a provider at its cap refuses quietly; an unavailable provider is logged", async () => {
    const { link, pool, runners, offer, log } = setup();
    link.interceptors.push((call) => call.op.case === "acquireLease"
      ? rejected(call.sessionId === "full" ? Code.PROVIDER_FULL : Code.PROVIDER_UNAVAILABLE, "provider") : undefined);
    offer("full");
    offer("gone");
    await waitFor(() => link.calls.length === 2, "acquires");
    await new Promise((resolve) => setTimeout(resolve, 10));
    expect(runners).toEqual([]);
    expect(pool.held).toBe(0);
    const warned = log.lines.filter((l) => l.message === "AcquireLease rejected").map((l) => l.fields?.session_id);
    expect(warned).toEqual(["gone"]);
  });

  test("LeaseRevoked drops the runner holding that epoch only", async () => {
    const { runners, hint, offer } = setup();
    offer("s1");
    await waitFor(() => runners.length === 1, "runner");
    hint({ case: "leaseRevoked", value: { sessionId: "s1", epoch: runners[0].epoch + 1n } });
    expect(runners[0].lost).toBeUndefined();
    hint({ case: "leaseRevoked", value: { sessionId: "s1", epoch: runners[0].epoch } });
    expect(runners[0].lost).toBe("lease revoked");
  });

  test("after a reconnect, sessions not resumed are dropped and stopped ones stopped", async () => {
    const { link, runners, offer } = setup();
    offer("a");
    offer("b");
    offer("c");
    await waitFor(() => runners.length === 3, "runners");
    const outcome = (r: StubRunner, ok: boolean, stopped = false) =>
      create(SessionReply_Resumed_OutcomeSchema, { sessionId: r.sessionId, ok, epoch: ok ? r.epoch : r.epoch, stopped });
    // A refused resume can carry the worker's own epoch: it is lost all the same.
    link.ready([outcome(runners[0], false), outcome(runners[1], true, true), outcome(runners[2], true)]);
    expect(runners[0].lost).toBe("lease not resumed");
    expect(runners[1].stopped).toBe(true);
    expect(runners[2].lost).toBeUndefined();
    void create(ResumeLeases_HeldSchema);
  });

  test("an Offer for a session being released is acquired again after the release", async () => {
    const { runners, offer } = setup();
    offer("s1");
    await waitFor(() => runners.length === 1, "runner");
    runners[0].ending = true;
    offer("s1");
    runners[0].end("released");
    await waitFor(() => runners.length === 2, "reacquired");
    expect(runners[1].sessionId).toBe("s1");
  });

  test("a session whose runner failed is not acquired again at once", async () => {
    const { runners, offer } = setup();
    offer("s1");
    await waitFor(() => runners.length === 1, "runner");
    runners[0].end("failed");
    await new Promise((resolve) => setTimeout(resolve, 10));
    offer("s1");
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(runners).toHaveLength(1);
  });

  test("shutdown releases every runner and takes no new lease", async () => {
    const { pool, runners, offer, acquires } = setup();
    offer("a");
    offer("b");
    await waitFor(() => runners.length === 2, "runners");
    await pool.shutdown(1_000);
    expect(runners.map((r) => r.shutdowns)).toEqual([1, 1]);
    offer("c");
    expect(acquires()).toHaveLength(2);
    expect(pool.held).toBe(0);
  });

  test("shutdown past its deadline drops running runners but not a release in flight", async () => {
    const { pool, runners, offer } = setup();
    offer("a");
    offer("b");
    await waitFor(() => runners.length === 2, "runners");
    runners[0].releaseMs = 200; // releasing, slower than the deadline
    runners[1].shutdown = () => { runners[1].shutdowns++; }; // never ends
    await pool.shutdown(50);
    expect(runners[0].lost).toBeUndefined();
    expect(runners[1].lost).toBe("shutdown deadline");
    expect(pool.held).toBe(0);
  });

  test("routes the link's hints", async () => {
    const { link, pool, runners } = setup();
    const running = pool.run();
    link.hint({ case: "offer", value: { sessionId: "s1" } });
    await waitFor(() => runners.length === 1, "runner");
    link.hintQueue.close();
    await running;
  });
});
