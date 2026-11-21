import type { NetConnectOpts } from "node:net";
import { expect, it, vi } from "vitest";
import { WireConnection } from "../src/wire/connection.js";
import { workerEndpoint } from "../src/wire/socket.js";
import { serve, workerOptions } from "./peer.js";

vi.mock("node:net", async (importOriginal) => {
  const net = await importOriginal<typeof import("node:net")>();
  return {
    ...net,
    createConnection(options: NetConnectOpts) {
      const socket = net.createConnection(options);
      Object.defineProperty(socket, "remoteAddress", { value: "192.0.2.1" });
      return socket;
    },
  };
});

it("checks the connected plaintext address before sending the upgrade or token", async () => {
  const server = await serve();
  const wire = new WireConnection({ ...workerOptions(server.url), endpoint: workerEndpoint(server.url) }, () => {});
  try {
    const outcome = await Promise.race([
      wire.ready.catch((error: unknown) => error),
      server.attempts.take(),
    ]);
    expect(outcome).toMatchObject({ message: "plaintext dial reached a non-loopback address" });
    expect(server.count).toBe(0);
  } finally {
    wire.fail(new Error("test finished"));
    await wire.done;
    await server.close();
  }
});
