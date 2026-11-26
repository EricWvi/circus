import { mkdir, open } from "node:fs/promises";
import { join } from "node:path";
import type { Readable } from "node:stream";

// A strict reader of npm package tarballs (ustar, with pax and GNU long
// names): it extracts the regular files and directories under "package/"
// into a directory and refuses everything else (links, devices, paths that
// leave the directory). File data is streamed to disk, never buffered whole.

const BLOCK = 512;
const MAX_ENTRIES = 4096;
const MAX_META_BYTES = 1 << 20;
const PREFIX = "package/";

export class TarError extends Error {
  override name = "TarError";
}

class ByteReader {
  #chunks: Buffer[] = [];
  #buffered = 0;
  #iterator: AsyncIterator<Buffer>;
  #ended = false;

  constructor(stream: Readable) {
    this.#iterator = stream[Symbol.asyncIterator]() as AsyncIterator<Buffer>;
  }

  async #fill(n: number): Promise<boolean> {
    while (this.#buffered < n && !this.#ended) {
      const next = await this.#iterator.next();
      if (next.done) {
        this.#ended = true;
        break;
      }
      this.#chunks.push(next.value);
      this.#buffered += next.value.length;
    }
    return this.#buffered >= n;
  }

  /** Exactly n bytes, or undefined at a clean end before any. */
  async read(n: number): Promise<Buffer | undefined> {
    if (!(await this.#fill(n))) {
      if (this.#buffered === 0) return undefined;
      throw new TarError("truncated tarball");
    }
    const all = this.#chunks.length === 1 ? this.#chunks[0] : Buffer.concat(this.#chunks);
    const out = all.subarray(0, n);
    const rest = all.subarray(n);
    this.#chunks = rest.length ? [rest] : [];
    this.#buffered = rest.length;
    return out;
  }

  /** n bytes as they arrive, in pieces. */
  async *pieces(n: number): AsyncGenerator<Buffer> {
    while (n > 0) {
      if (this.#buffered === 0 && !(await this.#fill(1))) throw new TarError("truncated tarball");
      const head = this.#chunks[0];
      const take = Math.min(n, head.length);
      yield head.subarray(0, take);
      if (take === head.length) this.#chunks.shift();
      else this.#chunks[0] = head.subarray(take);
      this.#buffered -= take;
      n -= take;
    }
  }

  async skip(n: number): Promise<void> {
    for await (const _ of this.pieces(n)) {
      // Discarded.
    }
  }
}

function text(block: Buffer, start: number, length: number): string {
  const field = block.subarray(start, start + length);
  const end = field.indexOf(0);
  return field.subarray(0, end < 0 ? length : end).toString("utf8");
}

function octal(block: Buffer, start: number, length: number): number {
  const field = block.subarray(start, start + length);
  if (field[0] & 0x80) {
    // Base-256: big files.
    let value = field[0] & 0x7f;
    for (let i = 1; i < length; i++) value = value * 256 + field[i];
    if (!Number.isSafeInteger(value)) throw new TarError("tar number too large");
    return value;
  }
  const digits = text(block, start, length).trim();
  if (!/^[0-7]*$/.test(digits)) throw new TarError("invalid tar number");
  return digits ? parseInt(digits, 8) : 0;
}

function checksumOk(block: Buffer): boolean {
  let sum = 0;
  for (let i = 0; i < BLOCK; i++) sum += i >= 148 && i < 156 ? 0x20 : block[i];
  return sum === octal(block, 148, 8);
}

function paxRecords(data: Buffer): Map<string, string> {
  const out = new Map<string, string>();
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    if (space < 0) throw new TarError("invalid pax header");
    const length = Number(data.subarray(pos, space).toString("ascii"));
    if (!Number.isInteger(length) || length <= space - pos || pos + length > data.length) throw new TarError("invalid pax header");
    const record = data.subarray(space + 1, pos + length - 1).toString("utf8");
    const eq = record.indexOf("=");
    if (eq < 0 || data[pos + length - 1] !== 0x0a) throw new TarError("invalid pax header");
    out.set(record.slice(0, eq), record.slice(eq + 1));
    pos += length;
  }
  return out;
}

/** The path below "package/", checked; undefined for "package/" itself. */
function relativePath(name: string): string[] | undefined {
  if (!name.startsWith(PREFIX)) throw new TarError(`tar entry outside package/: ${JSON.stringify(name.slice(0, 256))}`);
  const rest = name.slice(PREFIX.length).replace(/\/$/, "");
  if (rest === "") return undefined;
  const parts = rest.split("/");
  if (parts.some((part) => part === "" || part === "." || part === ".." || part.includes("\\") || part.includes("\0"))) {
    throw new TarError(`unsafe tar entry: ${JSON.stringify(name.slice(0, 256))}`);
  }
  return parts;
}

export interface ExtractOptions {
  /** The most file bytes the package may hold; checked before each file is written. */
  maxBytes?: number;
}

function formatSize(n: number): string {
  if (n >= 1 << 30 && n % (1 << 30) === 0) return `${n / (1 << 30)} GiB`;
  if (n >= 1 << 20 && n % (1 << 20) === 0) return `${n / (1 << 20)} MiB`;
  return `${n} bytes`;
}

/** Extracts the tarball read from stream (already decompressed) into dir, which must exist and be empty. */
export async function extractPackage(stream: Readable, dir: string, opts: ExtractOptions = {}): Promise<void> {
  const reader = new ByteReader(stream);
  const maxBytes = opts.maxBytes ?? Number.MAX_SAFE_INTEGER;
  let unpacked = 0;
  let longName: string | undefined;
  let pax = new Map<string, string>();
  for (let entries = 0; ; entries++) {
    if (entries > MAX_ENTRIES) throw new TarError("too many tar entries");
    const block = await reader.read(BLOCK);
    if (!block || block.every((byte) => byte === 0)) return;
    if (!checksumOk(block)) throw new TarError("tar header checksum mismatch");
    const type = String.fromCharCode(block[156] || 0x30);
    let size = octal(block, 124, 12);
    if (pax.has("size")) {
      size = Number(pax.get("size"));
      if (!Number.isSafeInteger(size) || size < 0) throw new TarError("invalid pax size");
    }
    const padding = (BLOCK - (size % BLOCK)) % BLOCK;
    if (type === "x" || type === "g" || type === "L") {
      if (size > MAX_META_BYTES) throw new TarError("tar metadata too large");
      const data = size ? await reader.read(size) : Buffer.alloc(0);
      if (!data) throw new TarError("truncated tarball");
      await reader.skip(padding);
      if (type === "x") pax = paxRecords(data);
      else if (type === "L") longName = data.toString("utf8").replace(/\0+$/, "");
      continue;
    }
    const prefix = block.subarray(257, 262).toString("ascii") === "ustar" ? text(block, 345, 155) : "";
    const header = prefix ? `${prefix}/${text(block, 0, 100)}` : text(block, 0, 100);
    const name = pax.get("path") ?? longName ?? header;
    pax = new Map();
    longName = undefined;
    const parts = relativePath(name);
    if (type === "5") {
      if (parts) await mkdir(join(dir, ...parts), { recursive: true, mode: 0o755 });
      await reader.skip(size + padding);
      continue;
    }
    if (type !== "0" && type !== "7") throw new TarError(`unsupported tar entry type ${JSON.stringify(type)}: ${JSON.stringify(name.slice(0, 256))}`);
    if (!parts) throw new TarError("tar file entry without a name");
    unpacked += size;
    if (unpacked > maxBytes) throw new TarError(`package larger than ${formatSize(maxBytes)} unpacked`);
    if (parts.length > 1) await mkdir(join(dir, ...parts.slice(0, -1)), { recursive: true, mode: 0o755 });
    // Executable bits are kept; group and others never write.
    const mode = octal(block, 100, 8) & 0o755 | 0o600;
    const file = await open(join(dir, ...parts), "wx", mode);
    try {
      for await (const piece of reader.pieces(size)) await file.write(piece);
      await file.chmod(mode);
      await file.sync();
    } finally {
      await file.close();
    }
    await reader.skip(padding);
  }
}
