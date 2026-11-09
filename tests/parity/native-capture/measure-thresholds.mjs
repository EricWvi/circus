import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { capture } from './capture.mjs';
import { thresholdCase, classify } from './threshold-cases.mjs';

// Every probe uses the same native capture code and its own fresh session.
// Probes stay in ignored .tmp; only the selected boundaries become fixtures.
const args = process.argv.slice(2);
const families = ['read-ascii-lines', 'read-long-line', 'read-cjk-lines', 'bash-ascii', 'bash-cjk'];
const selected = args.length ? args : families;
const initial = { 'read-ascii-lines': [3000, 100000], 'read-long-line': [5000, 1000000],
  'read-cjk-lines': [1000, 100000], 'bash-ascii': [1000, 100000], 'bash-cjk': [1000, 100000] };
const report = {};
const path = join(import.meta.dirname, '.tmp/threshold-results.json');
await mkdir(join(import.meta.dirname, '.tmp'), { recursive: true });
if (args.length) {
  try { Object.assign(report, JSON.parse(await readFile(path, 'utf8'))); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
}
async function save() { await writeFile(path, `${JSON.stringify(report, null, 2)}\n`); }
for (const family of selected) {
  if (!families.includes(family)) throw new Error(`Unknown threshold family: ${family}`);
  const probes = [];
  report[family] = { probes };
  async function probe(size) {
    const { fixture } = await capture(thresholdCase(family, size), { persist: false });
    const result = classify(fixture, family, size);
    probes.push(result);
    await save();
    console.log(`${family} ${size}: ${result.outcome} (${result.bytes} bytes, ${result.characters} characters)`);
    return result.outcome === 'inline';
  }
  let [low, high] = initial[family];
  if (!await probe(low)) throw new Error(`${family}: lower bracket is not inline`);
  while (await probe(high)) { low = high; high *= 2; }
  while (high - low > 1) {
    const middle = Math.floor((low + high) / 2);
    if (await probe(middle)) low = middle;
    else high = middle;
  }
  report[family].boundary = { largest_inline: low, smallest_not_inline: high };
  await save();
}
