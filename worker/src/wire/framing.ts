import { create, toBinary } from "@bufbuild/protobuf";
import type { Fragment } from "@circus/proto/circus/v1/link_pb";
import {
  WorkerDownlinkSchema, WorkerUplinkSchema, type WorkerDownlink,
} from "@circus/proto/circus/v1/worker_link_pb";
import { decodeEnvelope, FRAME_OVERHEAD, ProtocolError } from "./codec.js";

export interface Limits {
  messageBytes: number;
  fragmentBytes: number;
  inFlightBytes: number;
  intervalMs: number;
  timeoutMs: number;
}

export function fragmentFrame(
  bytes: Uint8Array, fragmentBytes: number, id: bigint, index: number,
): Uint8Array {
  const start = index * fragmentBytes;
  const data = bytes.subarray(start, start + fragmentBytes);
  return toBinary(WorkerUplinkSchema, create(WorkerUplinkSchema, {
    payload: {
      case: "fragment",
      value: {
        data, messageId: id, index, last: start + data.length === bytes.length,
        totalSize: index === 0 ? BigInt(bytes.length) : 0n,
      },
    },
  }));
}

const BLOCK_BYTES = 64 << 10;

interface PartialMessage {
  id: bigint;
  total: number;
  blocks: Buffer[];
  offset: number;
  next: number;
}

export class Receiver {
  #high = 0n;
  #partial?: PartialMessage;

  constructor(readonly limits: Limits) {}

  get partial(): boolean {
    return this.#partial !== undefined;
  }

  clear(): void {
    this.#partial = undefined;
  }

  receive(frame: Uint8Array, fragmentReceived: () => void): WorkerDownlink | undefined {
    const { fragmentBytes, messageBytes } = this.limits;
    const message = decodeEnvelope(WorkerDownlinkSchema, frame, fragmentBytes + FRAME_OVERHEAD, fragmentBytes);
    if (message.payload.case !== "fragment") {
      if (frame.length > fragmentBytes) throw new ProtocolError("unfragmented frame exceeds limit");
      return message;
    }
    const bytes = this.#add(message.payload.value);
    fragmentReceived();
    if (!bytes) return;
    const complete = decodeEnvelope(WorkerDownlinkSchema, bytes, messageBytes, fragmentBytes);
    if (complete.payload.case === "fragment") throw new ProtocolError("nested fragment");
    return complete;
  }

  #add(fragment: Fragment): Uint8Array | undefined {
    const { messageId, totalSize, index, last, data } = fragment;
    const { fragmentBytes, messageBytes, inFlightBytes } = this.limits;
    if (messageId === 0n || data.length === 0 || data.length > fragmentBytes) {
      throw new ProtocolError("invalid fragment ID or data size");
    }
    let partial = this.#partial;
    if (partial) {
      if (partial.id !== messageId) throw new ProtocolError("second partial message");
      if (totalSize !== 0n) throw new ProtocolError("total size on continuation");
    } else {
      if (messageId <= this.#high || index !== 0 || totalSize === 0n ||
        totalSize > BigInt(Math.min(messageBytes, inFlightBytes)) ||
        BigInt(data.length) > totalSize || last !== (BigInt(data.length) === totalSize)) {
        throw new ProtocolError("invalid first fragment");
      }
      partial = { id: messageId, total: Number(totalSize), blocks: [], offset: 0, next: 0 };
      this.#partial = partial;
      this.#high = messageId;
    }
    const end = partial.offset + data.length;
    if (index !== partial.next || end > partial.total || last !== (end === partial.total)) {
      throw new ProtocolError("invalid fragment index, size or last flag");
    }
    let copied = 0;
    while (partial.offset < end) {
      const index = Math.floor(partial.offset / BLOCK_BYTES);
      const offset = partial.offset % BLOCK_BYTES;
      const block = partial.blocks[index] ??= Buffer.allocUnsafe(Math.min(BLOCK_BYTES, partial.total - partial.offset));
      const count = Math.min(end - partial.offset, block.length - offset);
      block.set(data.subarray(copied, copied + count), offset);
      copied += count;
      partial.offset += count;
    }
    partial.next++;
    if (last) {
      this.clear();
      return partial.blocks.length === 1 ? partial.blocks[0] : Buffer.concat(partial.blocks, partial.total);
    }
  }
}
