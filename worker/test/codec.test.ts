import { create, toBinary } from "@bufbuild/protobuf";
import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire";
import { FragmentSchema, type Fragment } from "@circus/proto/circus/v1/link_pb";
import { SessionReplySchema, WorkerDownlinkSchema, WorkerUplinkSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { describe, expect, it, vi } from "vitest";
import {
  decodeEnvelope, encodeUplink, measureUplink, METADATA_BYTES, ProtocolError, TRANSCRIPT_BATCH_BYTES,
} from "../src/wire/codec.js";
import { fragmentFrame, Receiver } from "../src/wire/framing.js";

function frame(fragment: Partial<Omit<Fragment, "$typeName" | "$unknown">> = {}): Uint8Array {
  return toBinary(WorkerDownlinkSchema, create(WorkerDownlinkSchema, {
    payload: { case: "fragment", value: create(FragmentSchema, {
      messageId: 1n, totalSize: 6n, data: Buffer.from("1a0208", "hex"), ...fragment,
    }) },
  }));
}

const limits = { messageBytes: 200, inFlightBytes: 120, fragmentBytes: 8, intervalMs: 100, timeoutMs: 300 };

describe("worker downlink framing", () => {
  it.each([
    ["zero message ID", { messageId: 0n }],
    ["empty data", { data: new Uint8Array() }],
    ["oversized data", { data: new Uint8Array(9) }],
    ["missing declaration", { totalSize: 0n }],
    ["in-flight bytes", { totalSize: 121n }],
    ["message bytes", { totalSize: 201n }],
    ["nonzero initial index", { index: 1 }],
    ["declared size is smaller than chunk", { totalSize: 2n }],
    ["completion lacks last", { totalSize: 3n }],
    ["premature last", { last: true }],
  ] as const)("rejects %s", (_, input) => {
    expect(() => new Receiver(limits).receive(frame(input), () => {})).toThrow(ProtocolError);
  });

  it.each([
    ["different ID", { messageId: 2n }],
    ["continuation redeclaration", { totalSize: 1n }],
    ["index gap", { index: 2 }],
    ["end beyond total", { data: new Uint8Array(5) }],
    ["missing last", { last: false }],
  ] as const)("rejects %s on continuation", (_, input) => {
    const receiver = new Receiver(limits);
    expect(receiver.receive(frame({ totalSize: 4n }), () => {})).toBeUndefined();
    expect(() => receiver.receive(frame({
      index: 1, totalSize: 0n, last: true, data: Uint8Array.of(1), ...input,
    }), () => {})).toThrow(ProtocolError);
  });

  it("accepts short fragmented messages and forbids reusing the ID after completion", () => {
    const receiver = new Receiver(limits);
    receiver.receive(frame({ totalSize: 4n }), () => {});
    const full = frame({ index: 1, totalSize: 0n, data: Uint8Array.of(1), last: true });
    expect(receiver.receive(full, () => {})?.payload.case).toBe("heartbeat");
    expect(receiver.partial).toBe(false);
    expect(() => receiver.receive(frame(), () => {})).toThrow(ProtocolError);
  });

  it("reassembles a transcript page across the 64 KiB block boundary", () => {
    const json = "x".repeat(80 << 10) + "end";
    const message = create(WorkerDownlinkSchema, {
      requestId: 17n,
      payload: { case: "sessionReply", value: { result: { case: "transcriptPage", value: {
        entries: [{ json }], done: true,
      } } } },
    });
    const bytes = toBinary(WorkerDownlinkSchema, message);
    const fragmentBytes = 24 << 10;
    const receiver = new Receiver({ ...limits, fragmentBytes, messageBytes: 128 << 10, inFlightBytes: 128 << 10 });

    for (let offset = 0, index = 0; offset < bytes.length; offset += fragmentBytes, index++) {
      const data = bytes.subarray(offset, offset + fragmentBytes);
      const last = offset + data.length === bytes.length;
      const decoded = receiver.receive(frame({
        index, data, last, totalSize: index === 0 ? BigInt(bytes.length) : 0n,
      }), () => {});
      if (last) expect(decoded).toEqual(message);
      else expect(decoded).toBeUndefined();
      expect(receiver.partial).toBe(!last);
    }
  });

  it("rejects nested fragments and oversized unfragmented envelopes", () => {
    const receiver = new Receiver(limits);
    const nested = Uint8Array.from([0x22, 0]);
    expect(() => receiver.receive(frame({ totalSize: 2n, last: true, data: nested }), () => {}))
      .toThrow("nested fragment");
    expect(() => receiver.receive(Buffer.from("1a0710011801200128", "hex"), () => {}))
      .toThrow(ProtocolError);
    // Valid protobuf envelope, 10 bytes but the unfragmented cap is 8.
    expect(() => receiver.receive(Buffer.from("1a003206000000000000", "hex"), () => {}))
      .toThrow("unfragmented frame exceeds limit");
  });
});

function append(entries: { uuid?: string; json: string }[]) {
  return create(WorkerUplinkSchema, {
    requestId: 2n,
    payload: { case: "session", value: {
      sessionId: "s", epoch: 1n, op: { case: "appendTranscript", value: { entries, batchSeq: 1n } },
    } },
  });
}

describe("protobuf limits", () => {
  it("rejects an encoder result that disagrees with the measured size", () => {
    const message = append([{ json: "{}" }]);
    const finish = BinaryWriter.prototype.finish;
    const writer = vi.spyOn(BinaryWriter.prototype, "finish").mockImplementationOnce(function (this: BinaryWriter) {
      return finish.call(this).subarray(1);
    });
    try {
      expect(() => encodeUplink(message, METADATA_BYTES)).toThrow("uplink size changed during encoding");
    } finally {
      writer.mockRestore();
    }
  });

  it("counts ten varint bytes for a negative int64 mtime", () => {
    const patch = (mtimeUnixMs: bigint) => create(WorkerUplinkSchema, {
      requestId: 2n,
      payload: { case: "session", value: {
        sessionId: "s", epoch: 1n, op: { case: "patchState", value: {
          files: [{ device: "desk", path: "/file", mtimeUnixMs }],
        } },
      } },
    });
    const message = patch(-1n);
    // One field tag plus the sign-extended varint. Zero is omitted.
    expect(measureUplink(message, METADATA_BYTES) - measureUplink(patch(0n), METADATA_BYTES)).toBe(11);
    const bytes = encodeUplink(message, METADATA_BYTES);
    expect(decodeEnvelope(WorkerUplinkSchema, bytes, METADATA_BYTES)).toEqual(message);
  });

  it("checks complete append entry wire bytes including tags, with a single-entry exception", () => {
    const giant = { json: "a".repeat(TRANSCRIPT_BATCH_BYTES) };
    const cap = 2 * TRANSCRIPT_BATCH_BYTES;
    expect(encodeUplink(append([giant]), cap).length).toBeGreaterThan(TRANSCRIPT_BATCH_BYTES);
    expect(() => encodeUplink(append([giant, { json: "b" }]), cap)).toThrow("transcript batch size");
    expect(() => encodeUplink(append([giant]), METADATA_BYTES)).toThrow(ProtocolError);
  });

  it("checks the metadata limit and globally counts repeated fields and nodes", () => {
    const request = create(WorkerUplinkSchema, {
      payload: { case: "session", value: {
        sessionId: "s", epoch: 1n, op: { case: "patchState", value: {
          files: Array.from({ length: 32 }, () => ({ device: "desk", path: "/".repeat(4096) })),
        } },
      } },
    });
    expect(() => encodeUplink(request, 1 << 20)).toThrow(ProtocolError);
    expect(() => encodeUplink(append(Array(8190).fill({ json: "x" })), 1 << 20)).toThrow("repeated field limit");
    const state = create(SessionReplySchema, {
      result: { case: "state", value: { state: { cwds: Array.from({ length: 129 }, () => ({})) } } },
    });
    const stateBytes = toBinary(SessionReplySchema, state);
    const manyCwds = new BinaryWriter().tag(12, WireType.LengthDelimited)
      .bytes(Buffer.concat([stateBytes, stateBytes])).finish();
    expect(() => decodeEnvelope(WorkerDownlinkSchema, manyCwds, 1 << 20)).toThrow("repeated field limit");
    const fields = Buffer.alloc(2 * 65536, Buffer.from([0x28, 0]));
    expect(() => decodeEnvelope(WorkerDownlinkSchema, Buffer.concat([Buffer.from([0x62, 0]), fields]), 1 << 20))
      .toThrow("field count limit");
  });

  it("refuses malformed protobuf and outbound unpaired surrogates", () => {
    for (const hex of ["00", "1a", "1b1c", "1a0008ff", "1a001a00", "7a06520052005200", "7a00"]) {
      expect(() => decodeEnvelope(WorkerUplinkSchema, Buffer.from(hex, "hex"), 100)).toThrow(ProtocolError);
    }
    expect(() => encodeUplink(append([{ json: "\uD800" }]), 1024)).toThrow("invalid string");
    expect(() => encodeUplink(append([{ json: "ok", uuid: "é".repeat(65) }]), 1024)).toThrow("string size limit");
  });
});

it.each([
  "1a00a20100", // request_id as LEN
  "1a00a1010100000000000000", // request_id as fixed64
  "1a020a00", // Heartbeat.seq as LEN
  "1a050d01020304", // Heartbeat.seq as fixed32
])("ignores a known scalar with an undeclared wire type (%s)", (hex) => {
  expect(decodeEnvelope(WorkerDownlinkSchema, Buffer.from(hex, "hex"), METADATA_BYTES)).toMatchObject({
    requestId: 0n, payload: { case: "heartbeat", value: { seq: 0n } },
  });
});

it("preserves declared scalars after mismatched fields and updates nested lengths", () => {
  // Heartbeat originally needs a two-byte length prefix. Its ignored seq
  // field is followed by a valid sequence and leaves a one-byte length.
  const fields = Buffer.concat([Buffer.from([0x0a, 126]), Buffer.alloc(126), Buffer.from([0x08, 7])]);
  const wire = new BinaryWriter().tag(3, WireType.LengthDelimited).bytes(fields).finish();
  expect(decodeEnvelope(WorkerDownlinkSchema, wire, METADATA_BYTES)).toMatchObject({
    payload: { case: "heartbeat", value: { seq: 7n } },
  });
});


it("leaves the last flag clear when one more byte remains", () => {
  const bytes = new Uint8Array(33);
  const first = decodeEnvelope(WorkerUplinkSchema, fragmentFrame(bytes, 32, 1n, 0), 256, 32);
  const last = decodeEnvelope(WorkerUplinkSchema, fragmentFrame(bytes, 32, 1n, 1), 256, 32);
  expect(first.payload).toMatchObject({ case: "fragment", value: { last: false } });
  expect(last.payload).toMatchObject({ case: "fragment", value: { last: true } });
});

it("enforces a smaller metadata cap for worker events than the message cap", () => {
  const event = create(WorkerDownlinkSchema, {
    payload: { case: "workerEvent", value: { event: { case: "offer", value: { sessionId: "x".repeat(METADATA_BYTES) } } } },
  });
  expect(() => decodeEnvelope(WorkerDownlinkSchema, toBinary(WorkerDownlinkSchema, event), 1 << 20))
    .toThrow("payload size limit");
});

it("enforces the global message node limit even within individual repeated caps", () => {
  const lease = create(WorkerDownlinkSchema, {
    payload: { case: "sessionReply", value: { result: { case: "lease", value: {
      state: {
        files: Array.from({ length: 4096 }, () => ({})),
        jobs: Array.from({ length: 1024 }, () => ({ key: {} })),
        calls: Array.from({ length: 1040 }, () => ({})),
      },
      recovery: { calls: Array.from({ length: 1040 }, () => ({})) },
    } } } },
  });
  expect(() => decodeEnvelope(WorkerDownlinkSchema, toBinary(WorkerDownlinkSchema, lease), 1 << 20))
    .toThrow("message element limit");
});
