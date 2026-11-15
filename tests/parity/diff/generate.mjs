import { pathToFileURL } from 'node:url';
import { resolve } from 'node:path';
import { generateCombo } from './combo.mjs';

const bash = (command, extra = {}) => ({ tool: 'Bash', input: { command, description: 'Exercise shell behavior', ...extra } });
const read = (extra = {}) => ({ tool: 'Read', input: { file_path: '{{CWD}}/a.txt', ...extra } });
const write = (content, file_path = '{{CWD}}/a.txt') => ({ tool: 'Write', input: { file_path, content } });
const edit = (old_string, new_string, extra = {}) => ({ tool: 'Edit', input: { file_path: '{{CWD}}/a.txt', old_string, new_string, ...extra } });
const scenario = (kind, steps, files = {}, dirs = [], extra = {}) => ({ kind, setup: { files, dirs, ...extra }, steps });

function random(seed) {
  let state = 2166136261;
  for (const char of String(seed)) state = Math.imul(state ^ char.charCodeAt(0), 16777619) >>> 0;
  return () => { state += 0x6D2B79F5; let x = state; x = Math.imul(x ^ x >>> 15, x | 1); x ^= x + Math.imul(x ^ x >>> 7, x | 61); return ((x ^ x >>> 14) >>> 0) / 4294967296; };
}

// A shuffled coverage deck prevents a small run from sampling only happy paths.
function generateDeck({ seed = '1', count = 60 } = {}) {
  if (!Number.isSafeInteger(count) || count < 1) throw new Error('count must be a positive safe integer');
  const rng = random(seed);
  const pick = values => values[Math.floor(rng() * values.length)];
  const deck = [];
  const atom = () => pick(["printf 'alpha\\n'", "printf '中文😀'", "printf 'err\\n' >&2", 'true', 'false', "printf 'a\\rb\\r\\n'"]);
  const group = () => pick([`(${atom()})`, `{ ${atom()}; }`, atom()]);
  const shell = (kind, command, extra) => deck.push(() => scenario(kind, [bash(typeof command === 'function' ? command() : command, extra)]));
  shell('quotes', () => `printf '%s\\n' 'single "quote"' "double 'quote'" $'ansi\\tquote' back\\ slash ${pick(["'中文😀'", '"$HOME"', "'a;b|c'"])}`);
  shell('pipe', () => `${pick(["printf 'b\\na\\n'", "printf '中文😀\\n'", "printf 'z y x'"])} | ${pick(['sort', 'cat', "tr a A"])}`);
  shell('operators', () => `${group()} ${pick(['&&', '||', ';'])} ${group()}`);
  shell('groups', () => `${group()} ${pick([';', '&&', '||', '|'])} ${group()}`);
  shell('redirect', () => `${group()} > out; ${group()} >> out; cat out; ${atom()} 2>/dev/null`);
  shell('merge', "{ printf out; printf err >&2; } 2>&1");
  shell('heredoc', "cat <<'END'\nhello $HOME\nEND");
  shell('herestring', "cat <<< '中文 😀'");
  for (const code of [0, 1, 2, 127, 130]) shell(`exit-${code}`, `exit ${code}`);
  shell('stderr', "printf 'only error\\n' >&2");
  shell('interleaved', "printf a; printf b >&2; printf c; printf d >&2");
  shell('empty', 'true');
  shell('newlines', () => `printf ${pick(["'x'", "'x\\n'", "'x\\n\\n'", "'\\n\\n'"])}`);
  shell('crlf', "printf 'a\\rb\\r\\nc\\n'");
  shell('nul', "printf 'a\\0b\\n'");
  shell('invalid-utf8', "printf '\\377\\376x\\300\\257'");
  shell('unicode', "printf '中文😀 café\\n'");
  shell('ansi', "printf '\\033[31mred\\033[0m\\n'");
  for (const size of [29999, 30000, 30001]) shell(`bash-size-${size}`, `head -c ${size} /dev/zero | tr '\\0' x`);
  shell('bash-cjk-size', () => `head -c ${pick([9999, 10000, 10001])} /dev/zero | tr '\\0' x | sed 's/x/中/g'`);
  deck.push(() => scenario('cwd', [bash('cd sub; pwd'), bash('pwd')], {}, ['sub']));
  deck.push(() => scenario('export', [bash('export DIFF_VALUE=yes'), bash('printf "[%s]" "${DIFF_VALUE-unset}"')]));
  shell('errexit', 'set -e; false; printf unreachable');
  shell('nounset', 'set -u; printf "%s" "$DIFF_UNDEFINED"');
  shell('ampersand', '(sleep 0.05; printf child) & printf parent; wait');
  shell('sleep', () => `sleep ${pick(['0', '0.01', '0.05'])}; printf awake`, { timeout: 1000 });
  shell('timeout', 'sleep 0.2', { timeout: 50 });
  deck.push(() => scenario('background-stop', [bash('sleep 2', { run_in_background: true }), { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' }, when: 'task' }]));
  const contents = ['', 'alpha\nbeta\n', 'alpha\nbeta', 'alpha\r\nbeta\r\n', { base64: Buffer.from('\ufeffalpha\nbeta\n', 'utf16le').toString('base64') }, { base64: '/3gK' }, 'x'.repeat(5000)];
  contents.forEach((content, index) => deck.push(() => scenario(`read-encoding-${index}`, [read()], { 'a.txt': content })));
  for (const offset of [0, 1, 99, 4294967295]) deck.push(() => scenario(`read-offset-${offset}`, [read({ offset, limit: pick([0, 1, 2, 4294967295]) })], { 'a.txt': 'alpha\nbeta\n' }));
  for (const size of [262143, 262144, 262145]) deck.push(() => scenario(`read-size-${size}`, [read()], { 'a.txt': 'x'.repeat(size) }));
  deck.push(() => scenario('read-directory', [read({ file_path: '{{CWD}}/sub' })], {}, ['sub']));
  deck.push(() => scenario('read-missing', [read()]));
  deck.push(() => scenario('read-relative', [read({ file_path: 'a.txt', offset: 1, limit: 1 })], { 'a.txt': 'relative\n' }));
  for (const kind of ['new', 'overwrite', 'nested', 'empty', 'crlf', 'readonly']) deck.push(() => scenario(`write-${kind}`, [write(kind === 'empty' ? '' : kind === 'crlf' ? 'a\r\nb\r\n' : `value-${Math.floor(rng() * 100)}\n`, kind === 'nested' ? '{{CWD}}/one/two/a.txt' : '{{CWD}}/a.txt')], ['overwrite', 'readonly'].includes(kind) ? { 'a.txt': 'before\n' } : {}, [], kind === 'readonly' ? { modes: { 'a.txt': '0444' } } : {}));
  for (const kind of ['unique', 'multiple', 'all', 'missing', 'empty', 'same', 'crlf', 'curly', 'unread']) deck.push(() => {
    const content = kind === 'curly' ? '“quoted”\n' : kind === 'crlf' ? 'alpha\r\nbeta\r\n' : ['multiple', 'all'].includes(kind) ? 'beta\nbeta\n' : 'alpha\nbeta\n';
    const step = edit(kind === 'empty' ? '' : kind === 'missing' ? 'absent' : kind === 'curly' ? '"quoted"' : 'beta', kind === 'same' ? 'beta' : kind === 'curly' ? '"changed"' : 'BETA', { replace_all: kind === 'all' });
    return scenario(`edit-${kind}`, kind === 'unread' ? [step] : [read(), step], { 'a.txt': content });
  });
  const result = [];
  while (result.length < count) {
    const order = [...deck];
    for (let i = order.length - 1; i > 0; i--) { const j = Math.floor(rng() * (i + 1)); [order[i], order[j]] = [order[j], order[i]]; }
    for (const factory of order) {
      if (result.length === count) break;
      const { kind, ...definition } = factory();
      const seedID = Buffer.from(String(seed)).toString('hex');
      result.push({ id: `gen-${/^[a-zA-Z0-9_-]+$/.test(String(seed)) ? seed : seedID}-${result.length + 1}-${kind}`, description: `Generated ${kind} scenario with seed ${seed}.`, ...definition });
    }
  }
  return result;
}

export function generate({ mode = 'combo', seed = '1', count = 60 } = {}) {
  if (!Number.isSafeInteger(count) || count < 1) throw new Error('count must be a positive safe integer');
  if (mode === 'deck') return generateDeck({ seed, count });
  if (mode === 'combo') return generateCombo({ seed, count }, random(seed));
  throw new Error('mode must be combo or deck');
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const args = process.argv.slice(2);
  if (args.length % 2 || args.some((arg, i) => i % 2 === 0 && !['--seed', '--count', '--mode'].includes(arg))) throw new Error('Usage: node generate.mjs [--mode combo|deck] [--seed S] [--count N]');
  const options = Object.fromEntries(Array.from({ length: args.length / 2 }, (_, i) => [args[i * 2].slice(2), args[i * 2 + 1]]));
  console.log(JSON.stringify(generate({ ...options, count: Number(options.count ?? 60) }), null, 2));
}
