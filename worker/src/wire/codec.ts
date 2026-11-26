import { isUtf8 } from "node:buffer";
import {
  fromBinary, ScalarType, toBinary,
  type DescField, type DescMessage, type MessageShape,
} from "@bufbuild/protobuf";
import { reflect, type ReflectMessage } from "@bufbuild/protobuf/reflect";
import { WorkerUplinkSchema, type WorkerUplink } from "@circus/proto/circus/v1/worker_link_pb";

export const METADATA_BYTES = 64 << 10;
export const MESSAGE_BYTES = 64 << 20;
export const FRAGMENT_BYTES = 256 << 10;
export const FRAME_OVERHEAD = 128;
export const TRANSCRIPT_BATCH_BYTES = 8 << 20;
const MAX_NODES = 8192;
const MAX_FIELDS = 65536;
const MAX_DEPTH = 32;

export class ProtocolError extends Error {}

function requireWire(condition: unknown, message: string): asserts condition {
  if (!condition) throw new ProtocolError(message);
}

// Same per-envelope counters as internal/wire/charge.go for the worker schemas.
const repeatedLimits = new Map<string, number>(Object.entries({
  "CallUpdate.DeviceOffline.online_devices": 256,
  "ToolOutput.content": 128,
  "WorkerEvent.Nudge.reasons": 5,
  "SessionConfig.cwds": 256,
  "AppendTranscript.entries": MAX_NODES - 3,
  "ReportTurn.Persisted.input_ids": 32,
  "ReportTurn.Persisted.call_ids": 1024,
  "ReportTurn.Persisted.jobs": 1024,
  "ReportTurn.Persisted.late_call_ids": 16,
  "PatchSessionState.cwds": 256,
  "PatchSessionState.files": 4096,
  "PatchSessionState.returned_call_ids": 1024,
  "PatchSessionState.bgtasks_started": 64,
  "PatchSessionState.bgtasks_ended": 64,
  "PatchSessionState.handed_jobs": 1024,
  "PatchSessionState.awaiting_call_ids": 16,
  "GetSessionState.sections": 5,
  "SessionState.cwds": 256,
  "SessionState.files": 4096,
  "SessionState.jobs": 1024,
  "SessionState.calls": 1040,
  "SessionState.grants": 32,
  "Recovery.calls": 1040,
  "Recovery.lost_bgtasks": 64,
  "PublishEvents.events": 128,
  "ResumeLeases.leases": 64,
  "SessionReply.TranscriptPage.entries": MAX_NODES - 3,
  "SessionReply.TranscriptKeys.subpaths": 1024,
  "SessionReply.Inputs.inputs": 32,
  "SessionReply.Devices.devices": 256,
  "SessionReply.Resumed.leases": 64,
  "SessionReply.LaunchEnv.env": 72,
  "Attach.agents": 16,
  "ReportAgents.agents": 16,
  "WorkerEvent.AgentsWanted.agents": 16,
}));

const outboundStringLimits = new Map<string, number>(Object.entries({
  "Hello.boot_id": 128,
  "Attach.sdk_version": 256,
  "Attach.Account.email": 256,
  "Attach.Account.organization": 256,
  "Attach.Account.subscription_type": 256,
  "AgentStatus.installed_version": 64,
  "AgentStatus.target_version": 64,
  "AgentStatus.error": 1024,
  "SessionRequest.session_id": 128,
  "ResumeLeases.Held.session_id": 128,
  "LoadTranscript.subpath": 1024,
  "AppendTranscript.subpath": 1024,
  "TranscriptEntry.uuid": 128,
  "ReportTurn.usage_json": 4096,
  "ReportTurn.Persisted.input_ids": 128,
  "ReportTurn.Persisted.call_ids": 128,
  "ReportTurn.Persisted.late_call_ids": 128,
  "JobKey.device_id": 128,
  "JobKey.boot_id": 128,
  "JobKey.job_id": 128,
  "PatchSessionState.returned_call_ids": 128,
  "PatchSessionState.awaiting_call_ids": 128,
  "PatchSessionState.bgtasks_ended": 128,
  "PatchSessionState.Cwd.agent_id": 128,
  "PatchSessionState.Cwd.device": 128,
  "PatchSessionState.Cwd.cwd": 4096,
  "PatchSessionState.FileRecord.device": 128,
  "PatchSessionState.FileRecord.path": 4096,
  "PatchSessionState.FileRecord.sha256": 64,
  "PatchSessionState.BackgroundTask.task_id": 128,
  "PatchSessionState.BackgroundTask.tool_use_id": 128,
  "PatchSessionState.BackgroundTask.kind": 128,
  "PatchSessionState.BackgroundTask.description": 1024,
  "RequestApproval.tool_use_id": 128,
  "RequestApproval.agent_id": 128,
  "RequestApproval.tool": 256,
  "RequestApproval.device": 128,
  "RequestApproval.match_key": 64,
  "RequestApproval.display": 1024,
  "RequestApproval.reason": 1024,
  "ConsumeGrant.tool_use_id": 128,
  "ConsumeGrant.tool": 256,
  "ConsumeGrant.device": 128,
  "ConsumeGrant.match_key": 64,
}));

function nameOf(field: DescField): string {
  return field.parent.typeName.slice("circus.v1.".length) + "." + field.name;
}

class Budget {
  nodes = 0;
  fields = 0;
  repeated = new Map<DescField, number>();

  node(depth: number): void {
    requireWire(++this.nodes <= MAX_NODES && depth < MAX_DEPTH, "message element limit");
  }

  field(count: number, field?: DescField): void {
    this.fields += count;
    requireWire(this.fields <= MAX_FIELDS, "field count limit");
    if (field?.fieldKind !== "list") return;
    const limit = repeatedLimits.get(nameOf(field));
    if (limit === undefined) return;
    const total = (this.repeated.get(field) ?? 0) + count;
    requireWire(total <= limit, `repeated field limit: ${nameOf(field)}`);
    this.repeated.set(field, total);
  }
}

const fieldNumbers = new WeakMap<DescMessage, Map<number, DescField>>();

function fieldsFor(desc: DescMessage): Map<number, DescField> {
  let fields = fieldNumbers.get(desc);
  if (!fields) {
    fields = new Map(desc.fields.map((field) => [field.number, field]));
    fieldNumbers.set(desc, fields);
  }
  return fields;
}

function knownWireType(field: DescField): number {
  if (field.message || field.fieldKind === "map" ||
      (field.fieldKind === "list" && field.packed && field.listKind !== "enum")) return 2;
  switch (field.scalar) {
    case ScalarType.STRING:
    case ScalarType.BYTES: return 2;
    case ScalarType.DOUBLE:
    case ScalarType.FIXED64:
    case ScalarType.SFIXED64: return 1;
    case ScalarType.FLOAT:
    case ScalarType.FIXED32:
    case ScalarType.SFIXED32: return 5;
    default: return 0;
  }
}

interface Edit {
  start: number;
  end: number;
  replacement?: Uint8Array;
}

class Scanner extends Budget {
  #edits: Edit[] = [];
  #payload?: string;
  #sessionOp?: string;
  #appendBytes = 0;
  #appendEntries = 0;
  #resultBytes = 0;

  constructor(readonly maxBytes: number, readonly fragmentBytes: number) {
    super();
  }

  envelope(desc: DescMessage, bytes: Uint8Array): Uint8Array {
    requireWire(bytes.length <= this.maxBytes, "envelope size limit");
    const removed = this.#message(desc, bytes, 0, 0);
    requireWire(this.#payload !== undefined, "missing envelope payload");
    if (this.#payload === "session") {
      requireWire(this.#sessionOp !== undefined, "missing session operation");
      requireWire(
        this.#sessionOp === "append_transcript" || this.#sessionOp === "publish_events" ||
          bytes.length <= METADATA_BYTES,
        "session metadata size limit",
      );
      requireWire(
        this.#appendBytes <= TRANSCRIPT_BATCH_BYTES || this.#appendEntries === 1,
        "transcript batch size limit",
      );
    }
    if (this.#payload === "call_update") {
      requireWire(bytes.length - this.#resultBytes <= METADATA_BYTES, "status metadata size limit");
    }
    if (this.#edits.length === 0) return bytes;
    // Buf's decoder expects the declared wire type for known scalars. Go
    // treats those mismatches as unknown. Drop them before decoding, just as
    // we drop other unknown fields, while charging the original bytes/tags.
    const clean = Buffer.allocUnsafe(bytes.length - removed);
    let read = 0;
    let write = 0;
    for (const { start, end, replacement } of this.#edits.sort((a, b) => a.start - b.start)) {
      clean.set(bytes.subarray(read, start), write);
      write += start - read;
      if (replacement) {
        clean.set(replacement, write);
        write += replacement.length;
      }
      read = end;
    }
    clean.set(bytes.subarray(read), write);
    return clean;
  }

  #message(desc: DescMessage, bytes: Uint8Array, depth: number, offset: number): number {
    this.node(depth);
    let removed = 0;
    const fields = fieldsFor(desc);
    const cursor = new Cursor(bytes);
    while (cursor.pos < bytes.length) {
      const start = cursor.pos;
      const tag = cursor.varint(true);
      const number = Math.floor(tag / 8);
      const wire = tag % 8;
      requireWire(number > 0 && number <= 0x1fffffff, "invalid protobuf tag");
      const field = fields.get(number);
      this.field(1, field);

      if (depth === 0 && field?.oneof) {
        requireWire(!this.#payload && wire === 2, "envelope payload multiplicity or wire type");
        this.#payload = field.name;
        let limit = METADATA_BYTES;
        switch (field.name) {
          case "fragment": limit = this.fragmentBytes + FRAME_OVERHEAD; break;
          case "session":
          case "session_reply":
          case "call_update":
          case "submit_call": limit = this.maxBytes; break;
        }
        requireWire(bytes.length <= limit, "payload size limit");
      }
      if (desc.typeName === "circus.v1.SessionRequest" && field?.oneof) {
        requireWire(!this.#sessionOp, "multiple session operations");
        this.#sessionOp = field.name;
      }
      const mismatched = field !== undefined && wire !== knownWireType(field);
      if (mismatched && (field.message || field.fieldKind === "map" ||
        (field.fieldKind === "list" && field.listKind === "enum"))) {
        throw new ProtocolError("invalid message or repeated enum wire type");
      }

      switch (wire) {
        case 0: cursor.varint(false); break;
        case 1: cursor.skip(8); break;
        case 2: {
          const lengthStart = cursor.pos;
          const length = cursor.varint(true);
          const begin = cursor.pos;
          cursor.skip(length);
          const child = field?.message;
          if (child) {
            if (child.typeName === "circus.v1.DeviceResult") this.#resultBytes += length;
            if (child.typeName === "circus.v1.JobEvent") {
              requireWire(length <= METADATA_BYTES, "job event size limit");
            }
            if (desc.typeName === "circus.v1.AppendTranscript" && field.name === "entries") {
              this.#appendEntries++;
              this.#appendBytes += cursor.pos - start;
            }
            const childRemoved = this.#message(child, bytes.subarray(begin, cursor.pos), depth + 1, offset + begin);
            if (childRemoved) {
              const replacement = lengthPrefix(length - childRemoved);
              this.#edits.push({ start: offset + lengthStart, end: offset + begin, replacement });
              removed += childRemoved + begin - lengthStart - replacement.length;
            }
          } else if (!mismatched && field?.scalar === ScalarType.STRING) {
            requireWire(isUtf8(bytes.subarray(begin, cursor.pos)), "invalid UTF-8");
          }
          break;
        }
        case 5: cursor.skip(4); break;
        default: throw new ProtocolError("invalid protobuf wire type or group");
      }
      if (mismatched) {
        this.#edits.push({ start: offset + start, end: offset + cursor.pos });
        removed += cursor.pos - start;
      }
    }
    return removed;
  }
}

function lengthPrefix(n: number): Uint8Array {
  const bytes = [];
  while (n >= 128) {
    bytes.push((n & 127) | 128);
    n >>>= 7;
  }
  bytes.push(n);
  return Uint8Array.from(bytes);
}

class Cursor {
  pos = 0;

  constructor(readonly bytes: Uint8Array) {}

  // Only tags and lengths need their numeric value. Scalar uint64 values
  // are read by the generated decoder, after validating their full varint.
  varint(numeric: boolean): number {
    let value = 0;
    for (let i = 0; i < 10 && this.pos < this.bytes.length; i++) {
      const byte = this.bytes[this.pos++];
      if (i === 9 && byte > 1) break;
      if (numeric) value += (byte & 127) * 128 ** i;
      if (byte < 128) {
        requireWire(!numeric || value <= 0xffffffff, "protobuf tag or length overflow");
        return value;
      }
    }
    throw new ProtocolError("malformed protobuf varint");
  }

  skip(length: number): void {
    this.pos += length;
    requireWire(this.pos <= this.bytes.length, "truncated protobuf field");
  }
}

export function decodeEnvelope<S extends DescMessage>(
  schema: S, bytes: Uint8Array, maxBytes: number, fragmentBytes = FRAGMENT_BYTES,
): MessageShape<S> {
  const clean = new Scanner(maxBytes, fragmentBytes).envelope(schema, bytes);
  try {
    return fromBinary(schema, clean, { readUnknownFields: false, recursionLimit: MAX_DEPTH });
  } catch (cause) {
    throw new ProtocolError("malformed protobuf", { cause });
  }
}

function varintSize(value: bigint): number {
  if (value < 0n) return 10; // int32/int64 and negative enum values are sign-extended.
  let size = 1;
  while (value >= 128n) {
    value >>= 7n;
    size++;
  }
  return size;
}

function textSize(field: DescField, value: string, remaining: number): number {
  const name = nameOf(field);
  const cap = outboundStringLimits.get(name) ?? Infinity;
  requireWire(value.length <= remaining, "envelope size limit");
  requireWire(value.length <= cap, `string size limit: ${name}`);
  requireWire(value.isWellFormed(), `invalid string: ${name}`);
  const size = Buffer.byteLength(value);
  requireWire(size <= cap, `string size limit: ${name}`);
  if (field.name === "match_key") requireWire(/^[0-9a-f]{64}$/.test(value), "invalid match_key");
  if (field.name === "sha256") requireWire(/^[0-9a-fA-F]{64}$/.test(value), "invalid sha256");
  return size;
}

function measure(message: ReflectMessage, budget: Budget, depth: number, max: number): number {
  budget.node(depth);
  let size = 0;
  for (const field of message.fields) {
    if (!message.isSet(field)) continue;
    if (field.fieldKind === "map" || (field.fieldKind === "list" && field.packed)) {
      throw new ProtocolError("unsupported uplink field encoding");
    }
    const list = field.fieldKind === "list" ? message.get(field) : undefined;
    budget.field(list?.size ?? 1, field);
    const values = list ?? [message.get(field)];
    const wire = knownWireType(field);
    const tagSize = varintSize(BigInt(field.number * 8 + wire));
    const before = size;
    for (const value of values) {
      size += tagSize;
      switch (wire) {
        case 0:
          size += typeof value === "boolean" ? 1 : varintSize(BigInt(value as number | bigint));
          break;
        case 1: size += 8; break;
        case 5: size += 4; break;
        case 2: {
          const remaining = max - size;
          const length = field.message
            ? measure(value as ReflectMessage, budget, depth + 1, remaining)
            : field.scalar === ScalarType.STRING
              ? textSize(field, value as string, remaining)
              : (value as Uint8Array).byteLength;
          size += length + varintSize(BigInt(length));
          break;
        }
      }
      requireWire(size <= max, "envelope size limit");
    }
    if (message.desc.typeName === "circus.v1.AppendTranscript" && field.name === "entries") {
      requireWire(list?.size === 1 || size - before <= TRANSCRIPT_BATCH_BYTES, "transcript batch size limit");
    }
  }
  return size;
}

// Size the actual protobuf fields without allocating encoded data. PublishEvents
// has a count limit and M; 256 KiB is the server's view truncation threshold.
// The single-entry AppendTranscript exception also uses M.
export function measureUplink(message: WorkerUplink, maxBytes: number, fragmentBytes = FRAGMENT_BYTES): number {
  const payload = message.payload;
  requireWire(payload.case !== undefined, "missing envelope payload");
  let limit = Math.min(maxBytes, METADATA_BYTES);
  if (payload.case === "session") {
    const op = payload.value.op;
    requireWire(op.case !== undefined, "missing session operation");
    if (op.case === "appendTranscript" || op.case === "publishEvents") limit = maxBytes;
  } else if (payload.case === "fragment") {
    limit = Math.min(maxBytes, fragmentBytes + FRAME_OVERHEAD);
  }
  return measure(reflect(WorkerUplinkSchema, message), new Budget(), 0, limit);
}

export function encodeUplink(
  message: WorkerUplink, maxBytes: number, fragmentBytes = FRAGMENT_BYTES,
): Uint8Array {
  const size = measureUplink(message, maxBytes, fragmentBytes);
  const bytes = toBinary(WorkerUplinkSchema, message, { writeUnknownFields: false });
  requireWire(bytes.length === size, "uplink size changed during encoding");
  return bytes;
}
