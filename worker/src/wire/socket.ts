import type { RequestOptions } from "node:http";
import { BlockList, createConnection, isIP, type Socket } from "node:net";
import WebSocket, { type ClientOptions } from "ws";

const loopbacks = new BlockList();
loopbacks.addSubnet("127.0.0.0", 8);
loopbacks.addAddress("::1", "ipv6");

function isLoopback(address: string): boolean {
  const family = isIP(address);
  return family !== 0 && loopbacks.check(address, family === 4 ? "ipv4" : "ipv6");
}

export function workerEndpoint(base: string): URL {
  // Check the literal authority before URL normalizes aliases such as 127.1.
  const authority = /^[a-z]+:\/\/([^/?#]+)/i.exec(base)?.[1];
  if (!authority || /[?#\\\s]/.test(base) || authority.includes("@")) {
    throw new Error("invalid control_plane_url");
  }
  const url = new URL(base);
  switch (url.protocol) {
    case "https:":
    case "wss:":
      url.protocol = "wss:";
      break;
    case "http:":
    case "ws:": {
      const literal = authority.startsWith("[")
        ? authority.slice(1, authority.indexOf("]"))
        : authority.split(":")[0];
      if (literal !== "localhost" && !isLoopback(literal)) {
        throw new Error("plaintext worker links require a loopback host");
      }
      url.protocol = "ws:";
      break;
    }
    default:
      throw new Error("unsupported control_plane_url scheme");
  }
  url.pathname = url.pathname.replace(/\/+$/, "") + "/v1/link/worker";
  return url;
}

type Connect = (options: RequestOptions, callback: (error: Error | null, socket?: Socket) => void) => void;

// Node hands this socket to HTTP only after the connected address is checked.
// Until then HTTP has nowhere to write the upgrade headers or bearer token.
function connectLoopback(signal: AbortSignal): Connect {
  return ({ host, port }, callback) => {
    const socket = createConnection({ host: host ?? undefined, port: Number(port) });
    const abort = () => socket.destroy(new Error("worker dial aborted"));
    const onError = (error: Error) => {
      signal.removeEventListener("abort", abort);
      callback(error);
    };
    socket.once("error", onError);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
    socket.once("connect", () => {
      signal.removeEventListener("abort", abort);
      if (!socket.remoteAddress || !isLoopback(socket.remoteAddress)) {
        socket.destroy(new Error("plaintext dial reached a non-loopback address"));
        return;
      }
      // HTTP installs its error handler on nextTick after the callback.
      callback(null, socket);
      process.nextTick(() => socket.removeListener("error", onError));
    });
  };
}

type SocketOptions = Omit<ClientOptions, "agent" | "createConnection"> & {
  agent?: false;
  createConnection?: Connect;
  maxPendingAutoPongs: number;
};

type SocketInternals = {
  _socket?: Socket;
  _receiver?: {
    _maxPayload: number;
    _state: number;
    _bufferedBytes: number;
    _fragmented: number;
  };
};

export class LinkSocket extends WebSocket {
  rejectClose?: (code: number | undefined) => boolean;

  constructor(url: URL, token: string, ca: Buffer | undefined, maxPayload: number, signal: AbortSignal) {
    const opts: SocketOptions = {
      headers: { Authorization: "Bearer " + token },
      perMessageDeflate: false,
      followRedirects: false,
      allowSynchronousEvents: false,
      maxBufferedChunks: 65_536,
      maxFragments: 64,
      maxPendingAutoPongs: 16,
      maxPayload,
      // The ws types describe a synchronous net factory. Node also supports
      // the async HTTP callback used above when no agent is configured.
      ...(url.protocol === "ws:"
        ? { createConnection: connectLoopback(signal) }
        : { agent: false, minVersion: "TLSv1.2", rejectUnauthorized: true, ca }),
    };
    super(url, opts as ClientOptions);
  }

  get cleanEOF(): boolean {
    // ws reports both a TCP FIN and a reset as 1006. A FIN is graceful only
    // between complete RFC 6455 messages. State 0 is GET_INFO in ws 8.23.
    const { _socket: socket, _receiver: receiver } = this as WebSocket & SocketInternals;
    return socket?.readableEnded === true && receiver?._state === 0 &&
      receiver._bufferedBytes === 0 && receiver._fragmented === 0;
  }

  setReadLimit(bytes: number): void {
    // ws has no public post-upgrade setter. The pinned version checks this
    // Receiver limit before buffering payloads, even for RFC 6455 fragments.
    const receiver = (this as WebSocket & SocketInternals)._receiver;
    if (!receiver || typeof receiver._maxPayload !== "number") {
      throw new Error("ws receiver limit unavailable");
    }
    receiver._maxPayload = bytes;
  }

  override close(code?: number, data?: string | Buffer): void {
    // ws calls close() itself on malformed RFC 6455 frames and on a peer's
    // Close frame. Circus aborts protocol errors without a Close response.
    if (code !== 1000 || this.rejectClose?.(code)) {
      this.terminate();
      return;
    }
    super.close(code, data);
  }
}
