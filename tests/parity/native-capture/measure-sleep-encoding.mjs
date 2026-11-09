import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { capture } from './capture.mjs';
import { shellCase, encodingCases, bash } from './sleep-encoding-cases.mjs';

const dir = join(import.meta.dirname, '.tmp');
await mkdir(dir, { recursive: true });
const supplement = process.argv.includes('--supplement');
const units = process.argv.includes('--units');
const results = supplement || units ? JSON.parse(await readFile(join(dir, 'sleep-encoding-probes.json'), 'utf8')) : [];
async function probe(definition, group) {
  const { fixture } = await capture(definition, { persist: false });
  const first = fixture.steps[0].result;
  const blocked = Boolean(first?.is_error && first.content.startsWith('<tool_use_error>Blocked:'));
  results.push({ group, case: definition.id, blocked, fixture });
  await writeFile(join(dir, 'sleep-encoding-probes.json'), `${JSON.stringify(results, null, 2)}\n`);
  console.log(`${definition.id}: ${blocked ? 'BLOCKED' : first?.is_error ? 'ERROR' : 'ALLOWED'} ${JSON.stringify(first?.content.split('\n\n<system-reminder>\n<total_tokens>')[0]).slice(0, 500)}`);
  return blocked;
}

// Use a one-second execution timeout for guard probes: it does not alter the
// shell command, and allowed long sleeps need not run to completion.
if (!supplement && !units) {
let low = 1, high = 30;
await probe(shellCase('probe-sleep-1', 'sleep 1', { timeout: 1000 }), 'standalone');
if (!await probe(shellCase('probe-sleep-30', 'sleep 30', { timeout: 1000 }), 'standalone')) throw Error('Upper bracket was not blocked');
while (high - low > 1) {
  const n = Math.floor((low + high) / 2);
  if (await probe(shellCase(`probe-sleep-${n}`, `sleep ${n}`, { timeout: 1000 }), 'standalone')) high = n;
  else low = n;
}
for (const value of ['2.5', '1m', '0']) await probe(shellCase(`probe-sleep-${value}`, `sleep ${value}`, { timeout: 1000 }), 'standalone');
for (const separator of [';', '&&', '&']) for (const n of [low, high]) {
  await probe(shellCase(`probe-leading-${separator.replaceAll('&', 'and').replace(';', 'semicolon')}-${n}`,
    `sleep ${n} ${separator} echo x`, { timeout: 1000 }), 'leading');
}
for (const [name, command] of [
  ['echo-first', 'echo x; sleep 30'], ['cd-first', 'cd . && sleep 30'],
  ['subshell', '(sleep 30)'], ['pipe', 'sleep 30 | cat'],
  ['timeout-wrapper', 'timeout 30 sleep 30'], ['bash-wrapper', "bash -c 'sleep 30'"],
]) await probe(shellCase(`probe-${name}`, command, { timeout: 1000 }), 'shape');
for (const [name, command] of [['standalone', 'sleep 30'], ['leading', 'sleep 30; echo x']]) {
  await probe(shellCase(`probe-background-${name}`, command, { run_in_background: true }), 'background');
}
await probe(shellCase('probe-long-parameter', 'sleep 30', { timeout: 60000 }), 'timeout-parameter');

for (const [name, command, timeout = 1000] of [
  ['sleep', 'sleep 5'], ['stdout', 'echo before; sleep 5'], ['no-newline', 'printf x; sleep 5'],
  ['redirected', 'echo x >/dev/null; sleep 5'], ['true', 'true; sleep 5'],
  ['two-sleeps', 'sleep 0.5; sleep 5'], ['stderr', 'echo before >&2; sleep 5'],
  ['loop', 'for i in 1 2 3 4 5; do echo $i; sleep 1; done'],
  ['two-seconds', 'echo before; sleep 5', 2000],
]) await probe(shellCase(`probe-timeout-${name}`, command, { timeout }), 'timeout-background');
for (const definition of encodingCases) await probe(definition, 'encoding');
} else if (supplement) {
  for (const [name, command] of [
    ['sleep-then-output', 'sleep 0.1; echo x; sleep 5'],
    ['sleep-then-echo', 'sleep 5; echo x'],
    ['sleep-then-true', 'sleep 0.1 && true && sleep 5'],
    ['sleep-subshell', '(sleep 5)'],
  ]) await probe(shellCase(`probe-timeout-${name}`, command, { timeout: 1000 }), 'timeout-discriminator');
  await probe(shellCase('probe-timeout-lifecycle', 'echo before; sleep 5', { timeout: 1000 }, [
    bash('sleep 6', { timeout: 10000 }),
    { tool: 'Read', input: { file_path: '{{OUTPUT_PATH}}' } },
    { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' } },
  ]), 'lifecycle');
} else {
  for (const value of ['25s', '25m', '24.999', '25.0']) {
    await probe(shellCase(`probe-unit-${value}`, `sleep ${value}`, { timeout: 1000 }), 'unit-discriminator');
  }
}
