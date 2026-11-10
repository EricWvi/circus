import { open } from 'node:fs/promises';
import { createReadStream } from 'node:fs';
import { createHash } from 'node:crypto';

export function summarize(bytes) {
  return { format: 'sha256-bytes', length_bytes: bytes.length, sha256: createHash('sha256').update(bytes).digest('hex'),
    head_base64: bytes.subarray(0, 200).toString('base64'), tail_base64: bytes.subarray(-200).toString('base64') };
}
export async function summarizeFile(path) {
  const hash = createHash('sha256');
  let length = 0, head = Buffer.alloc(0), tail = Buffer.alloc(0);
  for await (const bytes of createReadStream(path)) {
    hash.update(bytes); length += bytes.length;
    if (head.length < 200) head = Buffer.concat([head, bytes]).subarray(0, 200);
    tail = Buffer.concat([tail, bytes]).subarray(-200);
  }
  return { format: 'sha256-bytes', length_bytes: length, sha256: hash.digest('hex'), head_base64: head.toString('base64'), tail_base64: tail.toString('base64') };
}
export async function generateFile(path, data) {
  if (!Number.isSafeInteger(data.size) || data.size < 0 || Buffer.byteLength(data.byte) !== 1) throw Error('Invalid file generator');
  if (data.line_bytes !== undefined && (!Number.isSafeInteger(data.line_bytes) || data.line_bytes < 2 || data.line_bytes > 1024 * 1024)) throw Error('Invalid generated line length');
  const file = await open(path, 'w');
  try {
    if (data.prefix) await file.write(data.prefix);
    const chunkSize = data.line_bytes ? Math.floor(1024 * 1024 / data.line_bytes) * data.line_bytes : 1024 * 1024;
    const chunk = Buffer.alloc(Math.min(data.size, chunkSize), data.byte);
    if (data.line_bytes) for (let i = data.line_bytes - 1; i < chunk.length; i += data.line_bytes) chunk[i] = 10;
    for (let remaining = data.size; remaining > 0; remaining -= chunk.length) await file.write(chunk.subarray(0, Math.min(remaining, chunk.length)));
    if (data.newline) await file.write('\n');
    const expected = data.size + Buffer.byteLength(data.prefix ?? '') + (data.newline ? 1 : 0);
    if ((await file.stat()).size !== expected) throw Error('Generated file has an unexpected byte length');
  } finally { await file.close(); }
}
