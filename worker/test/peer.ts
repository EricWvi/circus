import { once } from "node:events";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { create, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import type { WelcomeSchema } from "@circus/proto/circus/v1/link_pb";
import {
  WorkerDownlinkSchema, WorkerUplinkSchema, type WorkerUplink,
} from "@circus/proto/circus/v1/worker_link_pb";
import { expect } from "vitest";
import { WebSocket, WebSocketServer } from "ws";
import { type LinkOptions, WorkerLink } from "../src/link/index.js";
import { decodeEnvelope } from "../src/wire/codec.js";

const log = { debug() {}, info() {}, warn() {}, error() {} };

export function workerOptions(url: string): LinkOptions {
  return {
    url, principalId: "worker", token: "A".repeat(43), bootId: "01900000-0000-7000-8000-000000000001",
    clientVersion: "test", attach: { maxSessions: 64, sdkVersion: "", localOnly: true }, log,
  };
}

class Inbox<T> {
  #items: T[] = [];
  #pending?: (value: T) => void;

  push(item: T): void {
    if (this.#pending) {
      const pending = this.#pending;
      this.#pending = undefined;
      pending(item);
    } else {
      this.#items.push(item);
    }
  }

  take(): Promise<T> {
    if (this.#items.length) return Promise.resolve(this.#items.shift()!);
    if (this.#pending) throw new Error("inbox already has a pending read");
    return new Promise((resolve) => { this.#pending = resolve; });
  }
}

export class Peer {
  #inbox = new Inbox<WorkerUplink>();

  constructor(readonly ws: WebSocket) {
    ws.on("error", () => {});
    ws.on("message", (data, binary) => {
      expect(binary).toBe(true);
      expect(Buffer.isBuffer(data)).toBe(true);
      this.#inbox.push(decodeEnvelope(WorkerUplinkSchema, data as Buffer, 64 << 20));
    });
  }

  receive(): Promise<WorkerUplink> {
    return this.#inbox.take();
  }

  send(payload: MessageInitShape<typeof WorkerDownlinkSchema>["payload"], requestId = 0n): void {
    this.ws.send(toBinary(WorkerDownlinkSchema, create(WorkerDownlinkSchema, { requestId, payload })));
  }

  welcome(options: MessageInitShape<typeof WelcomeSchema> = {}): void {
    this.send({ case: "welcome", value: {
      principalId: "worker", negotiatedVersion: 1, maxInFlightMessages: 1,
      heartbeatIntervalMs: 1000, heartbeatTimeoutMs: 3000, ...options,
    } });
  }

  resume(request: WorkerUplink, outcomes: { sessionId: string; epoch: bigint; ok: boolean }[]): void {
    expect(request.payload.case).toBe("resumeLeases");
    this.send({ case: "sessionReply", value: {
      result: { case: "resumed", value: { leases: outcomes } },
    } }, request.requestId);
  }

  ok(requestId: bigint): void {
    this.send({ case: "sessionReply", value: { result: { case: "ok", value: {} } } }, requestId);
  }

  drop(): void {
    this.ws.terminate();
  }
}

/**
 * An upgrade answered with an HTTP status instead of the WebSocket: a
 * number is an anonymous answer (a proxy's), `refused` is the control
 * plane's own refusal (Circus-Auth: rejected).
 */
export type UpgradeAnswer = number | { refused: number };

export async function serve(statuses: UpgradeAnswer[] = []) {
  const server = createServer();
  const wss = new WebSocketServer({ noServer: true });
  const peers = new Inbox<Peer>();
  const attempts = new Inbox<number>();
  const links: WorkerLink[] = [];
  let count = 0;
  server.on("upgrade", (request, socket, head) => {
    expect(request.url).toBe("/base/v1/link/worker");
    expect(request.headers.authorization).toBe("Bearer " + "A".repeat(43));
    expect(request.headers.origin).toBeUndefined();
    expect(request.headers["sec-websocket-protocol"]).toBeUndefined();
    expect(request.headers["sec-websocket-extensions"]).toBeUndefined();
    attempts.push(++count);
    const answer = statuses.shift();
    if (answer) {
      const [status, marker] = typeof answer === "number" ? [answer, ""] : [answer.refused, "Circus-Auth: rejected\r\n"];
      socket.end(`HTTP/1.1 ${status} Test\r\n${marker}Content-Length: 0\r\nConnection: close\r\nLocation: /trap\r\n\r\n`);
    } else {
      wss.handleUpgrade(request, socket, head, (ws) => peers.push(new Peer(ws)));
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const url = `http://127.0.0.1:${(server.address() as AddressInfo).port}/base/`;
  return {
    url, attempts, peers, get count() { return count; },
    link(log?: Partial<LinkOptions["log"]>) {
      const opts = workerOptions(url);
      const link = new WorkerLink({ ...opts, log: { ...opts.log, ...log } });
      links.push(link);
      return link;
    },
    async close() {
      await Promise.all(links.map((link) => link.close()));
      for (const client of wss.clients) client.terminate();
      await Promise.all([
        new Promise<void>((resolve) => wss.close(() => resolve())),
        new Promise<void>((resolve) => server.close(() => resolve())),
      ]);
    },
  };
}

export async function hello(peer: Peer): Promise<WorkerUplink> {
  const request = await peer.receive();
  expect(request).toMatchObject({
    requestId: 0n,
    payload: { case: "hello", value: { bootId: workerOptions("").bootId, protocolVersion: 1 } },
  });
  return request;
}

export async function attach(peer: Peer): Promise<WorkerUplink> {
  const request = await peer.receive();
  expect(request).toMatchObject({
    requestId: 1n, payload: { case: "attach", value: { localOnly: true, maxSessions: 64 } },
  });
  return request;
}

