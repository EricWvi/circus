import { readFileSync, writeFileSync } from "node:fs";
import { create } from "@bufbuild/protobuf";
import { WorkerUplinkSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, it } from "vitest";
import { encodeUplink, ProtocolError } from "../src/wire/codec.js";
import { fragmentFrame, Receiver } from "../src/wire/framing.js";

const limits = { messageBytes: 1024, fragmentBytes: 32, inFlightBytes: 1024, intervalMs: 50, timeoutMs: 100 };
const transcript = "你好!".repeat(20);

interface Vector {
  name: string;
  frames: string[];
}

describe("Go worker wire vectors", () => {
  const fixtures = JSON.parse(readFileSync(new URL("vectors/go.json", import.meta.url), "utf8")) as Vector[];
  const [valid, ...invalid] = fixtures;
  it(valid.name, () => {
    const receiver = new Receiver(limits);
    const messages = valid.frames.flatMap((frame) => {
      const message = receiver.receive(Buffer.from(frame, "hex"), () => {});
      return message ? [message] : [];
    });
    expect(receiver.partial).toBe(false);
    expect(messages).toMatchObject([
      { requestId: 0n, payload: { case: "heartbeat", value: { seq: 1n, sentUnixMs: 42n } } },
      { requestId: 9007199254740995n, payload: { case: "sessionReply", value: {
        result: { case: "transcriptPage", value: {
          entries: [{ uuid: "entry", json: transcript }], nextSeq: 9007199254740999n, done: true,
        } },
      } } },
    ]);
  });

  for (const fixture of invalid) {
    it("rejects " + fixture.name, () => {
      const receiver = new Receiver(limits);
      expect(() => receiver.receive(Buffer.from(fixture.frames[0], "hex"), () => {})).toThrow(ProtocolError);
    });
  }
});

it("regenerates the TS uplink vector byte for byte", () => {
  const logical = encodeUplink(create(WorkerUplinkSchema, {
    requestId: 9007199254740993n,
    payload: { case: "session", value: {
      sessionId: "s-1", epoch: 7n, op: { case: "appendTranscript", value: {
        subpath: "agent/例", batchSeq: 1n, entries: [{ uuid: "entry", json: transcript }],
      } },
    } },
  }), limits.messageBytes, limits.fragmentBytes);
  const frames = [];
  for (let i = 0; i * limits.fragmentBytes < logical.length; i++) {
    frames.push(Buffer.from(fragmentFrame(logical, limits.fragmentBytes, 1n, i)).toString("hex"));
  }
  const fixture = new URL("vectors/ts.json", import.meta.url);
  const generated = JSON.stringify({ name: "transcript append", frames } satisfies Vector, null, 2) + "\n";
  if (process.env.UPDATE_WORKER_WIRE_VECTORS === "1") writeFileSync(fixture, generated);
  expect(readFileSync(fixture, "utf8")).toBe(generated);
});
