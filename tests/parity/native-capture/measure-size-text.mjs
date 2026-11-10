import { mkdir, writeFile } from 'node:fs/promises';
import { capture } from './capture.mjs';
import { sizeTextCase } from './size-text-cases.mjs';

const rows = [];
const output = new URL('./.tmp/size-text-probes.json', import.meta.url);
await mkdir(new URL('./.tmp/', import.meta.url), { recursive: true });
async function probe(size, multiline = false) {
  const { fixture } = await capture(sizeTextCase(size, multiline), { persist: false });
  const result = fixture.steps[0].result;
  const text = typeof result.content === 'string' ? result.content : JSON.stringify(result.content);
  const reported = text.match(/File content \(([^)]+)\)/)?.[1];
  if (!result.is_error || !reported) throw Error(`Unexpected result: ${text}`);
  rows.push({ size, structure: multiline ? '100-byte lines' : 'single line', reported, result });
  await writeFile(output, `${JSON.stringify(rows, null, 2)}\n`);
  console.log(`${size} ${multiline ? 'lines' : 'single'}: ${reported}`);
  return reported;
}
const sizes = [400000, 524288, 524289, 600000, 1048576, 2000000, 3200000, 3200001, 4000000, 8000000, 16777216, 33554432, 104857600];
for (const size of sizes) await probe(size);
// Above 524288 bytes, a reported 512KB cannot be the rounded real size.
let low = Math.max(...rows.filter(r => r.size > 600000 && r.reported !== '512KB').map(r => r.size));
let high = Math.min(...rows.filter(r => r.size > low && r.reported === '512KB').map(r => r.size));
if (!Number.isFinite(low) || !Number.isFinite(high)) throw Error('No measured bracket');
while (high - low > 1) {
  const middle = Math.floor((low + high) / 2);
  if (await probe(middle) === '512KB') high = middle;
  else low = middle;
}
await probe(low, true);
await probe(high, true);
console.log(`Exact boundary: ${low} / ${high}`);
