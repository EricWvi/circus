import { spawn } from 'node:child_process';
import { mkdir, readFile, writeFile, copyFile, realpath, stat } from 'node:fs/promises';
import { homedir } from 'node:os';
import { join, resolve, relative, isAbsolute } from 'node:path';
import { randomUUID } from 'node:crypto';
import { generate } from './generate.mjs';
import { repo, sessionCase, canonical } from './extract-sessions.mjs';

const tools = ['Bash', 'TaskStop', 'Read', 'Write', 'Edit'];
const json = value => JSON.stringify(value, null, 2) + '\n';
const usage = 'Usage: node run.mjs [--generated --mode combo|deck --seed S --count N] [--sessions corpus.jsonl --limit N] [--cases file.json] [--self-check] [--no-sandbox] [--run-dir DIR --promote ID]';
function parse(args) {
  const options = { mode: 'combo', seed: '1', count: 60, limit: 60, cases: [] };
  const flags = new Set(['generated', 'self-check', 'no-sandbox']);
  const values = new Set(['mode', 'seed', 'count', 'sessions', 'limit', 'cases', 'run-dir', 'promote']);
  for (let i = 0; i < args.length; i++) {
    const key = args[i].replace(/^--/, '');
    if (!args[i].startsWith('--')) throw new Error(usage);
    if (flags.has(key)) options[key] = true;
    else if (values.has(key) && args[i + 1] && !args[i + 1].startsWith('--')) {
      const value = args[++i];
      if (key === 'cases') options.cases.push(resolve(value));
      else options[key] = value;
    } else throw new Error(usage);
  }
  for (const key of ['count', 'limit']) {
    options[key] = Number(options[key]);
    if (!Number.isSafeInteger(options[key]) || options[key] < 1) throw new Error(`${key} must be a positive safe integer`);
  }
  if (!['combo', 'deck'].includes(options.mode)) throw new Error('mode must be combo or deck');
  if (options.sessions && options['no-sandbox']) throw new Error('Session corpus replay requires the sandbox');
  return options;
}

function execute(command, args, { cwd = repo, env = process.env, quiet = false } = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd, env, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => {
      output += chunk;
      if (!quiet) process.stdout.write(chunk);
    });
    child.on('error', reject);
    child.on('close', (code, signal) => resolve({ code, signal, output }));
  });
}

async function promote(options) {
  if (!options['run-dir'] || !/^[a-zA-Z0-9_-]+$/.test(options.promote)) throw new Error('Promotion requires --run-dir DIR and a plain case ID');
  const matches = [];
  for (const tool of tools) {
    const path = join(resolve(options['run-dir']), 'expected', tool, `${options.promote}.json`);
    try { const fixture = JSON.parse(await readFile(path, 'utf8')); if (fixture.case === options.promote) matches.push({ tool, path }); }
    catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
  if (matches.length !== 1) throw new Error('Expected exactly one captured fixture for promotion');
  const { tool, path } = matches[0];
  const target = join(repo, 'tests/parity/fixtures/native', tool, `${options.promote}.json`);
  await copyFile(path, target);
  console.log(`Promoted ${options.promote} to ${relative(repo, target)}`);
}

async function build() {
  const bin = join(repo, '.tmp/diff-bin');
  await mkdir(join(bin, 'tmp'), { recursive: true });
  for (const pkg of ['bashtool', 'fstools']) {
    console.log(`Building ${pkg} replay binary`);
    const result = await execute('go', ['test', '-c', '-o', join(bin, `${pkg}.test`), `./internal/daemon/${pkg}`], { env: { ...process.env, TMPDIR: join(bin, 'tmp') } });
    if (result.code !== 0) throw new Error(`${pkg} test build failed (${result.code})`);
  }
}

function within(base, path) {
  const rel = relative(base, path);
  return rel === '' || (!rel.startsWith('..') && !isAbsolute(rel));
}

async function sandbox(options, runDir) {
  const home = homedir();
  const args = ['--ro-bind', '/', '/', '--dev', '/dev', '--proc', '/proc', '--tmpfs', '/tmp', '--tmpfs', home,
    '--ro-bind', repo, repo];
  // The worktree's root node_modules link points into the main checkout.
  const modules = await realpath(join(repo, 'node_modules'));
  if (!within(repo, modules)) args.push('--ro-bind', modules, modules);
  // Keep the exact Node executable available when it lives under the masked home.
  const node = await realpath(process.execPath);
  if (within(home, node) && !within(repo, node)) args.push('--ro-bind', node, node);
  args.push('--bind', runDir, runDir, '--unshare-net', '--unshare-pid', '--die-with-parent', '--chdir', repo,
    node, resolve(import.meta.dirname, 'run.mjs'), ...process.argv.slice(2));
  const result = await execute('/usr/bin/bwrap', args, { env: {
    PATH: '/usr/local/bin:/usr/bin:/bin', HOME: home, LANG: 'C.UTF-8',
    CIRCUS_DIFF_WORKER: runDir, CIRCUS_DIFF_OPTIONS: JSON.stringify(options),
  } });
  if (result.code !== 0) throw new Error(`Sandbox failed (${result.code ?? result.signal}); no unsandboxed fallback is allowed`);
}

function validate(definitions) {
  const ids = new Set();
  for (const definition of definitions) {
    if (!/^[a-zA-Z0-9_-]+$/.test(definition.id) || ids.has(definition.id)) throw new Error('Case IDs must be unique plain filenames');
    ids.add(definition.id);
    if (!definition.setup?.files || !Array.isArray(definition.setup.dirs) || !definition.steps?.length || definition.steps.some(s => !tools.includes(s.tool))) throw new Error(`Invalid case definition: ${definition.id}`);
  }
}

function compact(output) {
  // Keep step/error lines and the two quoted difference snippets, not test chatter.
  return output.split('\n').filter(line => !/^(=== |--- |\s*--- |PASS$|FAIL$|\s*$)/.test(line)).join('\n').slice(0, 6000);
}

async function replay(definition, expected, runDir) {
  const mixed = definition.steps.some(s => ['Bash', 'TaskStop'].includes(s.tool));
  const pkg = mixed ? 'bashtool' : 'fstools';
  const tool = definition.steps[0].tool;
  const result = await execute(join(repo, '.tmp/diff-bin', `${pkg}.test`), [
    '-test.run', `^TestNativeParity$/${tool}$/${definition.id}$`, '-test.v', '-test.parallel=1', '-test.timeout=120s',
  ], { cwd: join(repo, 'internal/daemon', pkg), quiet: true, env: {
    PATH: '/usr/local/bin:/usr/bin:/bin', HOME: homedir(), LANG: 'C.UTF-8',
    CIRCUS_PARITY_ARTIFACT_DIR: join(runDir, 'daemon'), CIRCUS_PARITY_DIR: expected, CIRCUS_PARITY_TMPDIR: join(process.env.TMPDIR ?? '/tmp', 'circus-diff-replay'),
  } });
  await writeFile(join(runDir, 'replay', `${definition.id}.txt`), result.output, { mode: 0o600 });
  const ran = result.output.includes(`=== RUN   TestNativeParity/${tool}/${definition.id}\n`);
  if (!ran) throw new Error(`Replay did not discover ${definition.id}`);
  const deviations = result.output.split('\n').filter(line => line.includes('DEVIATION step'));
  return { status: result.code !== 0 ? 'mismatched' : deviations.length ? 'deviation' : 'matched',
    replay_exit_code: result.code, ...(deviations.length ? { deviations } : {}),
    ...(result.code !== 0 ? { diff: compact(result.output), steps: [...new Set([...result.output.matchAll(/step (\d+)/g)].map(m => Number(m[1])))] } : {}) };
}

const selfCase = () => ({ id: 'self-check', description: 'Verify filesystem and network isolation through native and daemon Bash.', setup: { files: {}, dirs: [] }, steps: [{ tool: 'Bash', input: {
  command: 'touch $HOME/escape; touch /tmp/escape; if curl --noproxy "*" --connect-timeout 2 --max-time 3 -sS https://example.com >/dev/null 2>&1; then printf NETWORK_ESCAPED; else printf NETWORK_BLOCKED; fi',
  description: 'Verify sandbox isolation',
} }] });

async function worker(options, runDir) {
  const started = performance.now();
  const expected = join(runDir, 'expected');
  for (const dir of [expected, join(runDir, 'replay'), join(runDir, 'diagnostics')]) await mkdir(dir, { recursive: true });
  // Import only after isolation is established. The SDK talks solely to the local fake API.
  const { capture } = await import('../native-capture/capture.mjs');
  const definitions = [];
  if (options.generated) definitions.push(...generate(options));
  for (const path of options.cases) {
    const value = JSON.parse(await readFile(path, 'utf8'));
    definitions.push(...(Array.isArray(value) ? value : [value]));
  }
  if (options.sessions) {
    const rows = (await readFile(options.sessions, 'utf8')).split('\n').filter(Boolean).slice(0, options.limit);
    definitions.push(...rows.map((line, i) => sessionCase(JSON.parse(line), i)));
  }
  if (options['self-check']) {
    if (!process.env.CIRCUS_DIFF_WORKER) throw new Error('The isolation self-check requires the sandbox');
    // The top-level HOME is masked too, independently of per-capture HOME trees.
    await writeFile(join(homedir(), 'escape'), 'sandbox only');
    let readonly = false;
    try { await writeFile(join(repo, '.tmp/diff-readonly-check'), 'escape'); }
    catch (error) { readonly = error.code === 'EROFS' || error.code === 'EACCES'; }
    if (!readonly) throw new Error('Repository mount is writable');
    definitions.push(selfCase());
  }
  if (!definitions.length) throw new Error('No cases selected');
  validate(definitions);
  const results = [];
  for (const [index, definition] of definitions.entries()) {
    const caseStart = performance.now();
    const entry = { case: definition.id, tool: definition.steps[0].tool };
    console.log(`[${index + 1}/${definitions.length}] ${definition.id}: native capture 1/2`);
    let first;
    try {
      first = await capture(definition, { persist: false, diagnosticsDir: join(runDir, 'diagnostics') });
      console.log(`[${index + 1}/${definitions.length}] ${definition.id}: native capture 2/2`);
      const second = await capture(definition, { persist: false, diagnosticsDir: join(runDir, 'diagnostics') });
      if (canonical(first) !== canonical(second)) {
        entry.status = 'unstable';
        await writeFile(join(runDir, 'diagnostics', `${definition.id}-unstable.json`), json({ first, second }), { mode: 0o600 });
      }
    } catch {
      // Capture writes the redacted local diagnostic. Never echo corpus commands.
      entry.status = 'capture-failed';
      entry.diagnostic = `diagnostics/${definition.id}-failure.txt`;
    }
    if (!entry.status) {
      const path = join(expected, entry.tool, `${definition.id}.json`);
      await mkdir(resolve(path, '..'), { recursive: true });
      await writeFile(path, json(first.fixture), { mode: 0o600 });
      console.log(`[${index + 1}/${definitions.length}] ${definition.id}: daemon replay`);
      Object.assign(entry, await replay(definition, expected, runDir));
      if (definition.id === 'self-check' && (!first.fixture.steps[0].result.content.startsWith('NETWORK_BLOCKED') || entry.status !== 'matched')) throw new Error('Isolation self-check failed');
    }
    entry.elapsed_ms = Math.round(performance.now() - caseStart);
    results.push(entry);
    console.log(`[${index + 1}/${definitions.length}] ${definition.id}: ${entry.status} (${entry.elapsed_ms} ms)`);
  }
  const counts = { total: results.length, matched: 0, mismatched: 0, unstable: 0, 'capture-failed': 0, deviation: 0 };
  for (const result of results) counts[result.status]++;
  const elapsed_ms = Math.round(performance.now() - started);
  const report = { counts, elapsed_ms, average_case_ms: Math.round(results.reduce((sum, r) => sum + r.elapsed_ms, 0) / results.length), sandboxed: Boolean(process.env.CIRCUS_DIFF_WORKER), results };
  await writeFile(join(runDir, 'report.json'), json(report), { mode: 0o600 });
  const markdown = ['# Differential parity report', '', ...Object.entries(counts).map(([key, value]) => `- ${key}: ${value}`), `- average case: ${report.average_case_ms} ms (two native captures and daemon replay)`, '',
    ...results.filter(r => r.status === 'mismatched').flatMap(r => [`## ${r.case}`, '', `Steps: ${r.steps.join(', ') || 'setup, final files, or replay envelope'}`, '', '```text', r.diff, '```', '']),
    ...results.filter(r => r.status === 'deviation').flatMap(r => [`## ${r.case}: deviation`, '', ...r.deviations, ''])].join('\n');
  await writeFile(join(runDir, 'report.md'), markdown + '\n', { mode: 0o600 });
  console.log(`Report: ${relative(repo, join(runDir, 'report.md'))}\n${JSON.stringify(counts)}`);
  if (options['self-check'] && counts.matched !== 1) throw new Error('Isolation self-check did not complete');
}

async function fingerprint(path) {
  try { const info = await stat(path); return `${info.ino}:${info.size}:${info.mtimeMs}`; }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
}

async function main() {
  const options = parse(process.argv.slice(2));
  if (process.env.CIRCUS_DIFF_WORKER) return worker(JSON.parse(process.env.CIRCUS_DIFF_OPTIONS), process.env.CIRCUS_DIFF_WORKER);
  if (options.promote) return promote(options);
  if (!options.generated && !options.sessions && !options.cases.length && !options['self-check']) throw new Error(usage);
  if (options['self-check'] && (options['no-sandbox'] || options.generated || options.sessions || options.cases.length)) throw new Error('Run --self-check alone');
  const base = join(repo, '.tmp/diff-runs');
  await mkdir(base, { recursive: true, mode: 0o700 });
  const runDir = join(base, `${new Date().toISOString().replace(/[:.]/g, '-')}-${randomUUID().slice(0, 8)}`);
  await mkdir(runDir, { mode: 0o700 });
  // Inputs can live anywhere under the read-only filesystem; copy them before HOME is masked.
  options.cases = await Promise.all(options.cases.map(async (path, index) => {
    const target = join(runDir, `cases-${index}.json`); await copyFile(path, target); return target;
  }));
  if (options.sessions) {
    const target = join(runDir, 'sessions.jsonl');
    await copyFile(resolve(options.sessions), target); options.sessions = target;
  }
  await build();
  if (options['no-sandbox']) {
    const temp = join(runDir, 'tmp');
    await mkdir(temp);
    process.env.TMPDIR = temp;
    return worker(options, runDir);
  }
  const escapes = [join(homedir(), 'escape'), '/tmp/escape', join(repo, '.tmp/diff-readonly-check')];
  const before = await Promise.all(escapes.map(fingerprint));
  await sandbox(options, runDir);
  const after = await Promise.all(escapes.map(fingerprint));
  if (canonical(before) !== canonical(after)) throw new Error('Sandbox modified an escape sentinel on the host');
  if (options['self-check']) console.log('Self-check passed: native and daemon network calls failed, HOME and /tmp writes stayed private, and the repository was read-only.');
}

try { await main(); } catch (error) { console.error(error.message); process.exitCode = 1; }
