import { once } from "node:events";
import type { Socket } from "node:net";
import { create, toBinary } from "@bufbuild/protobuf";
import { WorkerDownlinkSchema, WorkerUplinkSchema, type WorkerDownlink } from "@circus/proto/circus/v1/worker_link_pb";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { METADATA_BYTES, ProtocolError } from "../src/wire/codec.js";
import { WireConnection } from "../src/wire/connection.js";
import { LinkSocket, workerEndpoint } from "../src/wire/socket.js";
import { hello, serve, workerOptions, type Peer } from "./peer.js";

function heartbeat(peer: Peer, seq = 1n): void {
  peer.send({ case: "heartbeat", value: { seq } });
}

function partial(peer: Peer, id = 1n): void {
  peer.send({ case: "fragment", value: {
    messageId: id, totalSize: 4n, data: Buffer.from("1a0208", "hex"),
  } });
}

describe("worker transport", () => {
  let server: Awaited<ReturnType<typeof serve>>;
  let wire: WireConnection;
  let peer: Peer;
  let business: ReturnType<typeof vi.fn<(message: WorkerDownlink) => void>>;

  beforeEach(async () => {
    vi.useFakeTimers({ toFake: ["Date", "performance", "setTimeout", "clearTimeout"] });
    server = await serve();
    business = vi.fn<(message: WorkerDownlink) => void>();
    wire = new WireConnection({ ...workerOptions(server.url), endpoint: workerEndpoint(server.url) }, business);
    peer = await server.peers.take();
    await hello(peer);
  });

  afterEach(async () => {
    wire.fail(new Error("test finished"));
    await wire.ready.catch(() => {});
    await wire.done;
    await server.close();
    vi.useRealTimers();
  });

  async function marker(): Promise<void> {
    const received = Promise.withResolvers<void>();
    business.mockImplementationOnce(() => received.resolve());
    peer.send({ case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "s" } } } }, 1n);
    await received.promise;
  }

  it("sends heartbeat sequences and resets liveness on business frames, excluding WebSocket ping", async () => {
    peer.welcome({ heartbeatIntervalMs: 100, heartbeatTimeoutMs: 300 });
    await wire.ready;
    await vi.advanceTimersByTimeAsync(100);
    expect(await peer.receive()).toMatchObject({ payload: { case: "heartbeat", value: { seq: 1n } } });
    await vi.advanceTimersByTimeAsync(100);
    expect(await peer.receive()).toMatchObject({ payload: { case: "heartbeat", value: { seq: 2n } } });
    await marker();
    await vi.advanceTimersByTimeAsync(250);
    const pong = once(peer.ws, "pong");
    peer.ws.ping("WebSocket control");
    await pong;
    expect(wire.active).toBe(true);
    const closed = once(peer.ws, "close");
    await vi.advanceTimersByTimeAsync(50);
    await expect(wire.done).resolves.toMatchObject({ message: "heartbeat timeout" });
    expect((await closed)[0]).toBe(1006);
  });

  it("expires a partial message while other messages keep the connection alive", async () => {
    peer.welcome({ heartbeatIntervalMs: 100, heartbeatTimeoutMs: 300 });
    await wire.ready;
    partial(peer);
    await marker();
    await vi.advanceTimersByTimeAsync(200);
    heartbeat(peer);
    await marker();
    await vi.advanceTimersByTimeAsync(100);
    await expect(wire.done).resolves.toMatchObject({ message: "fragment timeout" });
  });

  it("renews fragment idle timeout on continuation rather than time since first fragment", async () => {
    peer.welcome({ heartbeatIntervalMs: 100, heartbeatTimeoutMs: 300 });
    await wire.ready;
    peer.send({ case: "fragment", value: { messageId: 1n, totalSize: 4n, data: Uint8Array.of(0x1a) } });
    await marker();
    await vi.advanceTimersByTimeAsync(200);
    peer.send({ case: "fragment", value: { messageId: 1n, index: 1, data: Uint8Array.of(0x02, 0x08) } });
    await marker();
    await vi.advanceTimersByTimeAsync(200);
    expect(wire.active).toBe(true);
    peer.send({ case: "fragment", value: { messageId: 1n, index: 2, last: true, data: Uint8Array.of(1) } });
    await marker();
    peer.ws.close(1000);
    await expect(wire.done).resolves.toBeUndefined();
  });

  it("bounds the entire upgrade and Hello/Welcome exchange", async () => {
    const timedOut = expect(wire.ready).rejects.toThrow("handshake timeout");
    await vi.advanceTimersByTimeAsync(9999);
    const closed = once(peer.ws, "close");
    await vi.advanceTimersByTimeAsync(1);
    await timedOut;
    expect((await closed)[0]).toBe(1006);
  });

  it("enforces the WebSocket read cap before Welcome", async () => {
    const closed = once(peer.ws, "close");
    peer.ws.send(Buffer.alloc(METADATA_BYTES + 1));
    await expect(wire.ready).rejects.toThrow();
    expect((await closed)[0]).toBe(1006);
  });

  it.each([
    ["text", (p: Peer) => p.ws.send("\x1a\x00")], // A valid Heartbeat envelope encoded as text.
    ["malformed protobuf", (p: Peer) => p.ws.send(Buffer.from("1a001a00", "hex"))],
    ["misencoded message field", (p: Peer) => p.ws.send(Buffer.from("6a021000", "hex"))],
    ["over F", (p: Peer) => p.ws.send(toBinary(WorkerDownlinkSchema, create(WorkerDownlinkSchema, {
      payload: { case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "x".repeat(70) } } } },
    })))],
    ["over F plus overhead", (p: Peer) => p.ws.send(Buffer.alloc(64 + 129))],
    ["second partial message", (p: Peer) => { partial(p); partial(p, 2n); }],
  ] as const)("aborts without a close frame on %s", async (kind, send) => {
    peer.welcome({ maxFragmentBytes: 64 });
    await wire.ready;
    const closed = once(peer.ws, "close");
    send(peer);
    const error = await wire.done;
    expect(error).toBeInstanceOf(kind === "over F plus overhead" ? RangeError : ProtocolError);
    expect((await closed)[0]).toBe(1006);
  });

  it("refuses a full raw FIFO locally while keeping the wire alive", async () => {
    peer.welcome();
    await wire.ready;
    const message = create(WorkerUplinkSchema, {
      requestId: 1n, payload: { case: "session", value: {
        sessionId: "s", epoch: 1n, op: { case: "takeInputs", value: {} },
      } },
    });
    for (let i = 0; i < 256; i++) wire.send(message);
    expect(() => wire.send(message)).toThrow("send queue full");
    expect(wire.active).toBe(true);
  });

  it("writes a due heartbeat between fragments of an uplink", async () => {
    peer.welcome({ maxFragmentBytes: 64, heartbeatIntervalMs: 100, heartbeatTimeoutMs: 300 });
    await wire.ready;
    const firstWrite = Promise.withResolvers<() => void>();
    const send = LinkSocket.prototype.send;
    const pause = vi.spyOn(LinkSocket.prototype, "send").mockImplementationOnce(function (
      this: LinkSocket, data, options, callback,
    ) {
      send.call(this, data, options, (error) => firstWrite.resolve(() => callback?.(error)));
    });
    try {
      wire.send(create(WorkerUplinkSchema, {
        requestId: 1n, payload: { case: "session", value: {
          sessionId: "s", epoch: 1n, op: { case: "appendTranscript", value: {
            batchSeq: 1n, entries: [{ json: "x".repeat(200) }],
          } },
        } },
      }));
      expect((await peer.receive()).payload).toMatchObject({ case: "fragment", value: { index: 0 } });
      await vi.advanceTimersByTimeAsync(100);
      (await firstWrite.promise)();
      expect((await peer.receive()).payload.case).toBe("heartbeat");
      expect((await peer.receive()).payload).toMatchObject({ case: "fragment", value: { index: 1 } });
    } finally {
      pause.mockRestore();
    }
  });

  it("filters incoming heartbeats from business delivery", async () => {
    peer.welcome();
    await wire.ready;
    heartbeat(peer);
    await marker();
    expect(business.mock.calls.map(([message]) => message.payload.case)).toEqual(["workerEvent"]);
  });

  it.each([199, 200])("rejects Welcome timeout %i when interval is 200", async (timeout) => {
    const invalid = expect(wire.ready).rejects.toThrow("invalid Welcome limits");
    peer.welcome({ heartbeatIntervalMs: 200, heartbeatTimeoutMs: timeout });
    await invalid;
  });

  it("reports an explicit non-1000 WebSocket close as an error", async () => {
    peer.welcome();
    await wire.ready;
    peer.ws.close(1001);
    await expect(wire.done).resolves.toMatchObject({ message: "worker WebSocket closed (1001)" });
  });

  it.each([false, true])("recognizes a clean TCP FIN only between complete frames (truncated=%s)", async (truncated) => {
    peer.welcome();
    await wire.ready;
    await marker();
    const socket = (peer.ws as typeof peer.ws & { _socket: Socket })._socket;
    if (truncated) {
      socket.end(Buffer.from([0x82, 0x05, 0x1a]));
      await expect(wire.done).resolves.toMatchObject({ message: "worker WebSocket closed (1006)" });
    } else {
      socket.end();
      await expect(wire.done).resolves.toBeUndefined();
    }
  });

  it("rejects a normal WebSocket close with a partial logical message", async () => {
    peer.welcome();
    await wire.ready;
    partial(peer);
    await marker();
    peer.ws.close(1000);
    await expect(wire.done).resolves.toMatchObject({ message: "closed with a partial message" });
  });

  it("stops heartbeats and bounds a close while waiting for a partial reply", async () => {
    peer.welcome({ heartbeatIntervalMs: 1000, heartbeatTimeoutMs: 10_000 });
    await wire.ready;
    partial(peer);
    await marker();
    wire.close();
    await vi.advanceTimersByTimeAsync(5000);
    await expect(wire.done).resolves.toMatchObject({ message: "worker close timeout" });
  });
});
