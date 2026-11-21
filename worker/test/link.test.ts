import { once } from "node:events";
import { create } from "@bufbuild/protobuf";
import {
  AcquireLeaseSchema, AppendTranscriptSchema, SessionReply_Rejected_Code, ReleaseLeaseSchema, ReleaseLease_Reason, WorkerEvent_Nudge_Reason as Reason,
  WorkerUplinkSchema,
} from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LinkLostError, TerminalLinkError, type ResumeOutcome, WorkerLink } from "../src/link/index.js";
import { decodeEnvelope, measureUplink } from "../src/wire/codec.js";
import { WireConnection } from "../src/wire/connection.js";
import { workerEndpoint } from "../src/wire/socket.js";
import { attach, hello, type Peer, serve, workerOptions } from "./peer.js";

const release = { case: "releaseLease", value: create(ReleaseLeaseSchema, { reason: ReleaseLease_Reason.IDLE }) } as const;

function ready(link: WorkerLink): Promise<ResumeOutcome[]> {
  return new Promise((resolve) => {
    const off = link.onReady((outcomes) => { off(); resolve(outcomes); });
  });
}

async function beforeResume(peer: Peer, welcome: Parameters<Peer["welcome"]>[0] = {}) {
  await hello(peer);
  peer.welcome(welcome);
  await attach(peer);
  return peer.receive();
}

async function initialize(peer: Peer, outcomes: { sessionId: string; epoch: bigint; ok: boolean }[] = []) {
  const request = await beforeResume(peer);
  peer.resume(request, outcomes);
  return request;
}

describe("WorkerLink operations", () => {
  let server: Awaited<ReturnType<typeof serve>>;

  beforeEach(async () => { server = await serve(); });
  afterEach(async () => { await server.close(); });

  it("accounts for Attach, rejects excess work, and reuses a slot behind Unwatch", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    await initialize(peer);
    expect(await attached).toEqual([]);

    const abort = new AbortController();
    const requests = Array.from({ length: 63 }, (_, i) =>
      link.session("s" + i, 1n, release, i === 0 ? abort.signal : undefined));
    const cancelled = expect(requests[0]).rejects.toThrow("unwatch me");
    await expect(link.session("overflow", 1n, release)).rejects.toThrow("slots exhausted");
    for (let i = 0; i < 63; i++) {
      expect(await peer.receive()).toMatchObject({
        requestId: BigInt(3 + i), payload: { case: "session", value: { sessionId: "s" + i } },
      });
    }
    abort.abort(new Error("unwatch me"));
    const next = link.session("after-abort", 1n, release);
    expect(await peer.receive()).toMatchObject({ requestId: 3n, payload: { case: "unwatch" } });
    const current = await peer.receive();
    expect(current.requestId).toBe(66n);
    peer.ok(3n); // A late reply cannot complete the new operation.
    peer.ok(999n);
    let resolved = false;
    void next.then(() => { resolved = true; });
    peer.ok(4n);
    await requests[1];
    expect(resolved).toBe(false);
    const afterReply = link.session("after-reply", 1n, release);
    expect((await peer.receive()).requestId).toBe(67n);
    for (let id = 5n; id <= 67n; id++) peer.ok(id);
    await Promise.all([cancelled, next, afterReply, ...requests.slice(2)]);
    await link.close();
    expect(peer.ws.readyState).toBe(peer.ws.CLOSED);
  });

  it("flushes same-session messages and Unwatch in FIFO order across fragments", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    await hello(peer);
    peer.welcome({ maxFragmentBytes: 64, maxMessageBytes: 1024n, maxInFlightBytes: 512n });
    await attach(peer);
    peer.resume(await peer.receive(), []);
    await attached;

    const append = (size: number) => ({
      case: "appendTranscript", value: create(AppendTranscriptSchema, {
        batchSeq: 1n, entries: [{ json: "x".repeat(size) }],
      }),
    }) as const;
    await expect(link.session("session", 1n, append(600))).rejects.toThrow("in-flight byte limit");
    await expect(link.session("session", 1n, append(1024))).rejects.toThrow("envelope size limit");
    const controller = new AbortController();
    const first = link.session("session", 1n, append(250), controller.signal);
    const aborted = expect(first).rejects.toThrow("unwatch it");
    const second = link.session("session", 1n, release);
    const lost = expect(second).rejects.toBeInstanceOf(LinkLostError);
    // Let the two grants enqueue their writes before canceling the first.
    await Promise.resolve();
    controller.abort(new Error("unwatch it"));
    const closing = link.close();

    const parts: Uint8Array[] = [];
    for (let index = 0; ; index++) {
      const { payload } = await peer.receive();
      expect(payload.case).toBe("fragment");
      if (payload.case !== "fragment") throw new Error("expected fragment");
      const f = payload.value;
      expect(f.messageId).toBe(1n);
      expect(f.index).toBe(index);
      if (!f.last) expect(f.data.length).toBe(64);
      parts.push(f.data);
      if (f.last) break;
    }
    expect(decodeEnvelope(WorkerUplinkSchema, Buffer.concat(parts), 1024)).toMatchObject({
      requestId: 5n, payload: { case: "session", value: { op: { case: "appendTranscript" } } },
    });
    expect(await peer.receive()).toMatchObject({ requestId: 6n, payload: { case: "session" } });
    expect(await peer.receive()).toMatchObject({ requestId: 5n, payload: { case: "unwatch" } });
    // The fixture leaves the second request unanswered before the orderly close.
    await lost;
    await closing;
    await aborted;
  });

  it("admits large appends one at a time before encoding and increases fragment IDs", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    const resume = await beforeResume(peer, { maxFragmentBytes: 64 << 10, maxMessageBytes: 1n << 20n });
    peer.resume(resume, []);
    await attached;

    const op = { case: "appendTranscript", value: create(AppendTranscriptSchema, {
      batchSeq: 1n, entries: [{ json: "x".repeat(900 << 10) }],
    }) } as const;
    const send = vi.spyOn(WireConnection.prototype, "send");
    try {
      const signal = AbortSignal.timeout(3000);
      const first = link.session("first", 1n, op, signal);
      const abort = new AbortController();
      const abandoned = link.session("abandoned", 1n, op, abort.signal);
      const canceled = expect(abandoned).rejects.toThrow("cancel queued append");
      const second = link.session("second", 1n, op, signal);
      const third = link.session("third", 1n, op, signal);
      await Promise.resolve();
      abort.abort(new Error("cancel queued append"));
      expect(send).toHaveBeenCalledTimes(1);
      await canceled;

      const requestIds: bigint[] = [];
      for (const [index, id] of ["first", "second", "third"].entries()) {
        const parts: Uint8Array[] = [];
        for (;;) {
          const { payload } = await peer.receive();
          expect(payload.case).toBe("fragment");
          if (payload.case !== "fragment") throw new Error("expected fragment");
          expect(payload.value.messageId).toBe(BigInt(index + 1));
          parts.push(payload.value.data);
          if (payload.value.last) break;
        }
        const request = decodeEnvelope(WorkerUplinkSchema, Buffer.concat(parts), 1 << 20);
        expect(request.payload).toMatchObject({ case: "session", value: { sessionId: id } });
        requestIds.push(request.requestId);
      }
      expect(send).toHaveBeenCalledTimes(3);
      for (const id of requestIds) peer.ok(id);
      for (const result of [first, second, third]) {
        await expect(result).resolves.toMatchObject({ result: { case: "ok" } });
      }
    } finally {
      send.mockRestore();
    }
  });

  it("keeps older granted work ahead of new requests after canceling a waiter", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    const resume = await beforeResume(peer, { maxFragmentBytes: 64 << 10, maxMessageBytes: 1n << 20n });
    peer.resume(resume, []);
    await attached;
    const op = { case: "appendTranscript", value: create(AppendTranscriptSchema, {
      batchSeq: 1n, entries: [{ json: "x".repeat(900 << 10) }],
    }) } as const;
    const signal = AbortSignal.timeout(3000);
    const abort = new AbortController();
    const first = link.session("big", 1n, op, signal);
    const blocked = link.session("blocked", 1n, op, abort.signal);
    const canceled = expect(blocked).rejects.toThrow("cancel the head");
    const older = link.session("ordered", 1n, release, signal);
    abort.abort(new Error("cancel the head"));
    const newer = link.session("ordered", 1n, release, signal);
    await canceled;
    for (;;) {
      const { payload } = await peer.receive();
      expect(payload.case).toBe("fragment");
      if (payload.case === "fragment" && payload.value.last) break;
    }
    expect((await peer.receive()).requestId).toBe(5n);
    expect((await peer.receive()).requestId).toBe(6n);
    for (const id of [3n, 5n, 6n]) peer.ok(id);
    await Promise.all([first, older, newer]);
  });

  it("keeps a small same-session request behind a waiting large append", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    const resume = await beforeResume(peer, { maxFragmentBytes: 256, maxMessageBytes: 256n });
    peer.resume(resume, []);
    await attached;

    const append = (batchSeq: bigint) => ({
      case: "appendTranscript", value: create(AppendTranscriptSchema, {
        batchSeq, entries: [{ json: "x".repeat(180) }],
      }),
    }) as const;
    const requests = [append(1n), append(2n), release].map((op) =>
      link.session("s", 1n, op, AbortSignal.timeout(3000)));
    const complete = Promise.all(requests);
    void complete.catch(() => {});
    const ids = [];
    for (let i = 0; i < requests.length; i++) ids.push((await peer.receive()).requestId);
    expect(ids).toEqual([3n, 4n, 5n]);
    for (const id of ids) peer.ok(id);
    await complete;
  });

  async function singleRequestBudget() {
    const sessionId = "s".repeat(96);
    // An exact fit makes even a leaked Unwatch reservation observable.
    const budget = measureUplink(create(WorkerUplinkSchema, {
      requestId: 3n, payload: { case: "session", value: { sessionId, epoch: 1n, op: release } },
    }), 256) + 16;
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    const resume = await beforeResume(peer, { maxFragmentBytes: budget, maxMessageBytes: BigInt(budget) });
    peer.resume(resume, []);
    await attached;
    return { link, peer, sessionId };
  }

  it("returns a grant when the operation aborts before the send microtask", async () => {
    const { link, peer, sessionId } = await singleRequestBudget();
    const send = vi.spyOn(WireConnection.prototype, "send");
    try {
      const abort = new AbortController();
      const reason = new Error("abort before send");
      const canceled = expect(link.session(sessionId, 1n, release, abort.signal)).rejects.toBe(reason);
      abort.abort(reason);
      const next = link.session(sessionId, 1n, release, AbortSignal.timeout(3000));
      void next.catch(() => {});

      await canceled;
      expect(send).toHaveBeenCalledOnce();
      const message = await peer.receive();
      expect(message).toMatchObject({ requestId: 4n, payload: { case: "session" } });
      peer.ok(message.requestId);
      await next;
    } finally {
      send.mockRestore();
    }
  });

  it.each(["reply", "Unwatch"] as const)("returns the whole grant only after the relevant write completes (%s)", async (retire) => {
    const { link, peer, sessionId } = await singleRequestBudget();
    const held = Promise.withResolvers<() => void>();
    const send = WireConnection.prototype.send;
    const spy = vi.spyOn(WireConnection.prototype, "send").mockImplementation(function (
      this: WireConnection, message, size, written,
    ) {
      // Let the peer read the frame while delaying the completion notification.
      const hold = retire === "reply"
        ? spy.mock.calls.length === 1
        : message.payload.case === "unwatch";
      send.call(this, message, size, hold ? () => held.resolve(() => written?.()) : written);
    });
    try {
      const abort = new AbortController();
      const first = link.session(sessionId, 1n, release, abort.signal);
      void first.catch(() => {});
      const request = await peer.receive();
      const next = link.session(sessionId, 1n, release, AbortSignal.timeout(3000));
      void next.catch(() => {});

      if (retire === "reply") {
        peer.ok(request.requestId);
        await expect(first).resolves.toMatchObject({ result: { case: "ok" } });
      } else {
        const reason = new Error("abandon sent request");
        const canceled = expect(first).rejects.toBe(reason);
        abort.abort(reason);
        expect(await peer.receive()).toMatchObject({ requestId: request.requestId, payload: { case: "unwatch" } });
        await canceled;
      }

      const finish = await held.promise;
      const callsBeforeWrite = retire === "reply" ? 1 : 2;
      expect(spy).toHaveBeenCalledTimes(callsBeforeWrite);
      finish();
      await Promise.resolve();
      expect(spy).toHaveBeenCalledTimes(callsBeforeWrite + 1);
      const message = await peer.receive();
      expect(message.requestId).toBe(4n);
      peer.ok(message.requestId);
      await next;
    } finally {
      spy.mockRestore();
    }
  });

  it("rejects an oversized admission locally and still sends other operations", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    await initialize(peer);
    await attached;
    const send = vi.spyOn(WireConnection.prototype, "send");
    try {
      const op = { case: "appendTranscript", value: create(AppendTranscriptSchema, {
        batchSeq: 1n, entries: [{ json: "x".repeat(32 << 20) }],
      }) } as const;
      await expect(link.session("s", 1n, op)).rejects.toThrow("send byte budget");
      expect(send).not.toHaveBeenCalled();
      const after = link.session("s", 1n, release);
      peer.ok((await peer.receive()).requestId);
      await expect(after).resolves.toMatchObject({ result: { case: "ok" } });
    } finally {
      send.mockRestore();
    }
  });

  it("deduplicates AcquireLease in flight for the same session only", async () => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    await initialize(peer);
    await attached;

    const acquire = { case: "acquireLease", value: create(AcquireLeaseSchema, { fresh: true }) } as const;
    const first = link.session("s", 0n, acquire);
    await expect(link.session("s", 0n, acquire)).rejects.toThrow("AcquireLease already in flight");
    const other = link.session("other", 0n, acquire);
    const answer = async (sessionId: string) => {
      const m = await peer.receive();
      expect(m.payload).toMatchObject({ case: "session", value: { sessionId } });
      peer.send({ case: "sessionReply", value: { result: { case: "lease", value: { epoch: 1n } } } }, m.requestId);
    };
    await answer("s");
    await answer("other");
    await Promise.all([first, other]);
    const again = link.session("s", 0n, acquire);
    await answer("s");
    await again;
  });

  it.each(["workerEvent", "wrong result"] as const)("rejects %s for an open one-shot request", async (kind) => {
    const link = server.link();
    const attached = ready(link);
    link.start();
    const peer = await server.peers.take();
    await initialize(peer);
    await attached;
    await expect(link.session("s", 0n, release)).rejects.toThrow("invalid session ID or epoch");
    const pending = link.session("s", 1n, release);
    const lost = expect(pending).rejects.toMatchObject({
      name: "LinkLostError",
      cause: { message: kind === "workerEvent" ? "worker event for a one-shot operation" : "unexpected session reply" },
    });
    const { requestId } = await peer.receive();
    peer.send(kind === "workerEvent"
      ? { case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "s" } } } }
      : { case: "sessionReply", value: { result: { case: "turn", value: { turnNo: 1n } } } }, requestId);
    await lost;
  });

  it("coalesces hints, preserves distinct kinds, and holds delivery until resume", async () => {
    const link = server.link();
    const attached = ready(link);
    const hints = link.hints()[Symbol.asyncIterator]();
    const firstHint = hints.next();
    expect(() => link.hints()[Symbol.asyncIterator]()).toThrow("already have a consumer");
    await expect(hints.next()).rejects.toThrow("pending read");
    const ordering: string[] = [];
    void attached.then(() => { ordering.push("ready"); });
    void firstHint.then(() => { ordering.push("hint"); });
    link.setHeld("held", 9n);
    link.start();
    const peer = await server.peers.take();
    await hello(peer);
    peer.welcome();
    await attach(peer);
    const resume = await peer.receive();
    expect(resume).toMatchObject({ requestId: 2n, payload: { value: { leases: [{ sessionId: "held", epoch: 9n }] } } });
    for (let i = 0; i < 300; i++) {
      peer.send({ case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "s" } } } }, 1n);
    }
    for (const [reasons, interruptTurnNo] of [[
      [Reason.INPUT, Reason.INTERRUPT], 4n,
    ], [[Reason.INPUT, Reason.STOP], 2n]] as const) {
      peer.send({ case: "workerEvent", value: { event: { case: "nudge", value: {
        sessionId: "s", reasons: [...reasons], interruptTurnNo,
      } } } }, 1n);
    }
    peer.send({ case: "workerEvent", value: { event: { case: "leaseRevoked", value: {
      sessionId: "s", epoch: 2n,
    } } } }, 1n);
    peer.resume(resume, [{ sessionId: "held", epoch: 9n, ok: true }]);
    expect(await attached).toMatchObject([{ sessionId: "held", epoch: 9n, ok: true }]);
    expect((await firstHint).value).toMatchObject({ event: { case: "offer" } });
    expect(ordering).toEqual(["ready", "hint"]);
    expect((await hints.next()).value).toMatchObject({
      event: { case: "nudge", value: { reasons: [Reason.INPUT, Reason.INTERRUPT, Reason.STOP], interruptTurnNo: 4n } },
    });
    expect((await hints.next()).value).toMatchObject({ event: { case: "leaseRevoked", value: { epoch: 2n } } });
    const waiting = hints.next();
    await hints.return?.();
    expect((await waiting).done).toBe(true);
  });
});

describe("WorkerLink reconnect", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["Date", "performance", "setTimeout", "clearTimeout"] });
    vi.spyOn(Math, "random").mockReturnValue(0.999999);
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("backs off from 500 ms to 10 seconds and stops on HTTP 403", async () => {
    const server = await serve([503, 302, 503, 503, 503, 503, 503, 403]);
    const warn = vi.fn();
    const opts = workerOptions(server.url);
    const monitored = new WorkerLink({ ...opts, log: { ...opts.log, warn } });
    monitored.start();
    try {
      for (const [index, delay] of [500, 1000, 2000, 4000, 8000, 10000, 10000].entries()) {
        expect(await server.attempts.take()).toBe(index + 1);
        await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(index + 1));
        expect(warn.mock.lastCall?.[1].retry_in_ms).toBe(delay);
        await vi.advanceTimersByTimeAsync(delay - 1);
        expect(server.count).toBe(index + 1);
        await vi.advanceTimersByTimeAsync(1);
      }
      await expect(monitored.closed).rejects.toThrow("HTTP 403");
      expect(server.count).toBe(8);
    } finally {
      await monitored.close();
      await server.close();
    }
  });

  it("fails pending requests with LinkLostError and resumes the same boot before work", async () => {
    const server = await serve();
    const link = server.link();
    const first = ready(link);
    link.start();
    try {
      const peer = await server.peers.take();
      await initialize(peer);
      await first;
      link.setHeld("held", 9n);
      link.setHeld("gone", 10n);
      const reply = link.session("held", 9n, release);
      const lost = expect(reply).rejects.toBeInstanceOf(LinkLostError);
      expect((await peer.receive()).requestId).toBe(3n);
      const again = ready(link);
      peer.drop();
      await lost;
      await expect(link.session("held", 9n, release)).rejects.toBeInstanceOf(LinkLostError);
      await vi.advanceTimersByTimeAsync(500);
      const next = await server.peers.take();
      await hello(next);
      next.welcome();
      await attach(next);
      const resume = await next.receive();
      expect(resume).toMatchObject({ requestId: 2n, payload: { value: {
        leases: [{ sessionId: "held", epoch: 9n }, { sessionId: "gone", epoch: 10n }],
      } } });
      await expect(link.session("held", 9n, release)).rejects.toBeInstanceOf(LinkLostError);
      next.resume(resume, [{ sessionId: "held", epoch: 9n, ok: true }, { sessionId: "gone", epoch: 11n, ok: false }]);
      expect(await again).toMatchObject([{ epoch: 9n, ok: true }, { epoch: 11n, ok: false }]);
      const retried = link.session("held", 9n, release);
      const request = await next.receive();
      expect(request.requestId).toBe(3n);
      next.ok(request.requestId);
      await expect(retried).resolves.toMatchObject({ result: { case: "ok" } });
    } finally {
      await server.close();
    }
  });

  it.each(["http401", "reject", "principal", "version", "count", "message size"] as const)(
    "treats %s as terminal", async (failure) => {
      const server = await serve(failure === "http401" ? [401] : []);
      const link = server.link();
      link.start();
      try {
        if (failure !== "http401") {
          const peer = await server.peers.take();
          await hello(peer);
          if (failure === "reject") peer.send({ case: "reject", value: { code: 4 } });
          else peer.welcome({
            principalId: failure === "principal" ? "elsewhere" : "worker",
            negotiatedVersion: failure === "version" ? 2 : 1,
            maxInFlightMessages: failure === "count" ? 2 : 1,
            maxMessageBytes: failure === "message size" ? (64n << 20n) + 1n : 0n,
          });
        }
        await expect(link.closed).rejects.toBeInstanceOf(TerminalLinkError);
        await vi.advanceTimersByTimeAsync(45_000);
        expect(server.count).toBe(1);
      } finally {
        await server.close();
      }
    });

  it("redials on normal backoff when the peer drops a resume request", async () => {
    const server = await serve();
    const warn = vi.fn();
    const link = server.link({ warn });
    link.start();
    try {
      const peer = await server.peers.take();
      expect((await beforeResume(peer)).payload.case).toBe("resumeLeases");
      peer.drop();
      await vi.waitFor(() => expect(warn).toHaveBeenCalledWith("worker link disconnected",
        expect.objectContaining({ retry_in_ms: 500 })));
      await vi.advanceTimersByTimeAsync(500);
      await initialize(await server.peers.take());
    } finally {
      await server.close();
    }
  });

  it("discards hints from an abandoned resume attempt", async () => {
    const server = await serve();
    const warn = vi.fn();
    const link = server.link({ warn });
    const hints = link.hints()[Symbol.asyncIterator]();
    const waiting = hints.next();
    link.start();
    try {
      const peer = await server.peers.take();
      await beforeResume(peer);
      peer.send({ case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "stale" } } } }, 1n);
      // The pong proves the worker already read the preceding hint.
      const pong = new Promise((resolve) => peer.ws.once("pong", resolve));
      peer.ws.ping();
      await pong;
      peer.drop();
      await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(500);
      const next = await server.peers.take();
      const request = await beforeResume(next);
      next.send({ case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "fresh" } } } }, 1n);
      next.resume(request, []);
      await expect(waiting).resolves.toMatchObject({ value: { event: { value: { sessionId: "fresh" } } } });
    } finally {
      await hints.return?.();
      await server.close();
    }
  });

  it("resumes the latest held set and prunes a failed outcome even at the requested epoch", async () => {
    const server = await serve();
    const link = server.link();
    const first = ready(link);
    link.setHeld("old", 5n);
    link.start();
    try {
      const peer = await server.peers.take();
      const request = await beforeResume(peer);
      link.setHeld("old", null);
      link.setHeld("new", 7n);
      peer.resume(request, [{ sessionId: "old", epoch: 5n, ok: true }]);
      const repeated = await peer.receive();
      expect(repeated.payload).toMatchObject({ case: "resumeLeases", value: {
        leases: [{ sessionId: "new", epoch: 7n }],
      } });
      peer.resume(repeated, [{ sessionId: "new", epoch: 7n, ok: false }]);
      expect(await first).toMatchObject([{ sessionId: "new", epoch: 7n, ok: false }]);
      peer.drop();
      await vi.waitFor(() => expect(peer.ws.readyState).toBe(peer.ws.CLOSED));
      await vi.advanceTimersByTimeAsync(550);
      const next = await server.peers.take();
      const resume = await beforeResume(next);
      expect(resume.payload).toMatchObject({ case: "resumeLeases", value: { leases: [] } });
      next.resume(resume, []);
    } finally {
      await server.close();
    }
  });

  it.each([
    ["count", [], "worker lease resumption failed"],
    ["epoch", [{ sessionId: "s", epoch: 9n, ok: true }], "invalid lease resumption outcome"],
    ["id", [{ sessionId: "other", epoch: 7n, ok: true }], "invalid lease resumption outcome"],
    ["rejected", null, "STORE_UNAVAILABLE"],
  ] as const)("refuses readiness on an invalid resume %s", async (_, outcomes, error) => {
    const server = await serve();
    const warn = vi.fn();
    const link = server.link({ warn });
    const ready = vi.fn();
    link.onReady(ready);
    link.setHeld("s", 7n);
    link.start();
    try {
      const peer = await server.peers.take();
      const resume = await beforeResume(peer);
      if (outcomes) peer.resume(resume, [...outcomes]);
      else peer.send({ case: "sessionReply", value: { result: {
        case: "rejected", value: { code: SessionReply_Rejected_Code.STORE_UNAVAILABLE },
      } } }, resume.requestId);
      await vi.waitFor(() => expect(warn).toHaveBeenCalledWith("worker link disconnected",
        expect.objectContaining({ error: expect.stringContaining(error) })));
      expect(ready).not.toHaveBeenCalled();
    } finally {
      await server.close();
    }
  });

  it("times out an unanswered resume after 10 seconds even with an active wire", async () => {
    const server = await serve();
    const warn = vi.fn();
    const link = server.link({ warn });
    link.start();
    try {
      const peer = await server.peers.take();
      await beforeResume(peer, { heartbeatIntervalMs: 11_000, heartbeatTimeoutMs: 30_000 });
      await vi.advanceTimersByTimeAsync(9999);
      expect(warn).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1);
      await vi.waitFor(() => expect(warn).toHaveBeenCalledWith("worker link disconnected",
        expect.objectContaining({ error: expect.stringContaining("resumption timeout") })));
    } finally {
      await server.close();
    }
  });

  it.each([[59_000, 1000], [60_000, 500]])("uses the stability threshold at %i ms", async (uptime, afterDrop) => {
    const server = await serve([503]);
    const warn = vi.fn();
    const link = server.link({ warn });
    const attached = ready(link);
    link.start();
    try {
      await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(1));
      await vi.advanceTimersByTimeAsync(500);
      const peer = await server.peers.take();
      const resume = await beforeResume(peer, { heartbeatIntervalMs: 30_000, heartbeatTimeoutMs: 120_000 });
      peer.resume(resume, []);
      await attached;
      await vi.advanceTimersByTimeAsync(uptime);
      peer.drop();
      await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(2));
      expect(warn.mock.lastCall?.[1].retry_in_ms).toBe(afterDrop);
    } finally {
      await server.close();
    }
  });

  it("jitter can choose the lower half of the retry interval", async () => {
    vi.mocked(Math.random).mockReturnValue(0.25);
    const server = await serve([503]);
    const warn = vi.fn();
    const link = server.link({ warn });
    link.start();
    try {
      await vi.waitFor(() => expect(warn).toHaveBeenCalledTimes(1));
      expect(warn.mock.lastCall?.[1].retry_in_ms).toBe(312);
    } finally {
      await server.close();
    }
  });

  it("can stop during the raw handshake", async () => {
    const server = await serve();
    const link = server.link();
    link.start();
    try {
      const peer = await server.peers.take();
      await hello(peer);
      const closed = once(peer.ws, "close");
      await link.close();
      expect((await closed)[0]).toBe(1006);
    } finally {
      await server.close();
    }
  });
});

it("caps the held lease table at 64 sessions", async () => {
  const link = new WorkerLink(workerOptions("http://127.0.0.1"));
  for (let i = 0; i < 64; i++) link.setHeld("s" + i, 1n);
  expect(() => link.setHeld("s64", 1n)).toThrow("too many held leases");
  link.setHeld("s0", 2n);
  link.setHeld("s0", null);
  link.setHeld("s64", 1n);
  await link.close();
});

it("checks the literal loopback host and rejects credential-bearing or redirected base URLs", () => {
  expect(workerEndpoint("http://[::ffff:127.0.0.2]:9/base").href).toBe("ws://[::ffff:7f00:2]:9/base/v1/link/worker");
  expect(workerEndpoint("https://control.example/base//").href).toBe("wss://control.example/base/v1/link/worker");
  for (const url of ["http://127.1", "http://example.test", "http://localhost.evil", "https://user@host",
    "https://host?#", "https://host\\path", "ftp://127.0.0.1"]) {
    expect(() => workerEndpoint(url)).toThrow();
  }
});

describe("interrupted lease resumption", () => {
  it("returns from close while a resume reply is outstanding", async () => {
    const server = await serve();
    const link = server.link();
    link.start();
    try {
      const peer = await server.peers.take();
      await hello(peer);
      peer.welcome();
      await attach(peer);
      expect((await peer.receive()).payload.case).toBe("resumeLeases");
      const started = performance.now();
      await link.close();
      expect(performance.now() - started).toBeLessThan(1000);
    } finally {
      await server.close();
    }
  }, 12_000);
});
