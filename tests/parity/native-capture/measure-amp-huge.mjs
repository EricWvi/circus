import { readFile, writeFile } from 'node:fs/promises';
import { capture } from './capture.mjs';
import { ampCases, hugeCases, hugeCase } from './amp-huge-cases.mjs';

const bisect = process.argv.includes('--bisect');
const path = `${import.meta.dirname}/.tmp/amp-huge-probes.json`;
const rows = bisect ? JSON.parse(await readFile(path, 'utf8')) : [];
async function probe(c) {
  try {
    const { fixture } = await capture(c, { persist: false });
    const observation = JSON.parse(await readFile(`${import.meta.dirname}/.tmp/${c.id}-observations.json`, 'utf8'));
    const timings = JSON.parse(await readFile(`${import.meta.dirname}/.tmp/${c.id}-timings.json`, 'utf8'));
    rows.push({ case: c.id, fixture, observation, timings });
    console.log(`PASS ${c.id}: ${fixture.steps.map(s => s.result.is_error)}, peak VmHWM ${observation.max_vm_hwm_kib} KiB`);
    return !fixture.steps[0].result.is_error;
  } catch (error) {
    rows.push({ case: c.id, error: error.message });
    console.error(`FAIL ${c.id}: ${error.message}`);
    throw error;
  } finally { await writeFile(path, `${JSON.stringify(rows, null, 2)}\n`); }
}
if (!bisect) {
  for (const c of [...ampCases, ...hugeCases]) await probe(c);
} else {
  let low = 1048576, high = 10485760;
  const byId = new Map(rows.map(row => [row.case, row]));
  if (byId.get('read-huge-line-1m-limit')?.fixture.steps[0].result.is_error !== false ||
      byId.get('read-huge-line-10m-limit')?.fixture.steps[0].result.is_error !== true) {
    throw Error('The measured sizes do not bracket a behavior change');
  }
  while (high - low > 1) {
    const middle = Math.floor((low + high) / 2);
    if (await probe(hugeCase(`probe-${middle}`, middle, { limit: 1 }))) low = middle;
    else high = middle;
  }
  console.log(`Largest successful payload: ${low}; smallest error payload: ${high}`);
}
