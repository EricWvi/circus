import { query } from '@anthropic-ai/claude-agent-sdk';
import { mkdir, mkdtemp, writeFile, readFile, readdir, rm, stat } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve, relative } from 'node:path';
import { createHash } from 'node:crypto';
import { cases } from './cases.mjs';
import { startFakeApi } from './fake-api.mjs';

const root = import.meta.dirname;
const fixtures = resolve(root, '../fixtures/native');
const model = 'claude-sonnet-4-6';
const toolNames = ['Bash', 'Read', 'Write', 'Edit', 'TaskStop'];
const args = process.argv.slice(2);
if (args.some((arg, i) => arg !== '--case' && args[i - 1] !== '--case') || args.at(-1) === '--case') {
  throw new Error('Usage: node capture.mjs [--case <glob>]');
}
const patterns = args.filter((_, i) => args[i - 1] === '--case');
const glob = pattern => new RegExp(`^${pattern.split('*').map(s => s.split('?').map(x => x.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('.')).join('.*')}$`);
const selected = cases.filter(c => !patterns.length || patterns.some(p => glob(p).test(c.id)));
if (!selected.length) throw new Error('No matching cases');
await mkdir(fixtures, { recursive: true });
await mkdir(join(root, '.tmp'), { recursive: true });

const json = value => `${JSON.stringify(value, null, 2)}\n`;
const digest = value => createHash('sha256').update(value).digest('hex');
const replaceData = (value, fn) => typeof value === 'string' ? fn(value) : Array.isArray(value)
  ? value.map(v => replaceData(v, fn)) : value && typeof value === 'object'
    ? Object.fromEntries(Object.entries(value).map(([k, v]) => [k, replaceData(v, fn)])) : value;

async function capture(definition) {
  const base = await mkdtemp(join(tmpdir(), 'circus-native-'));
  const cwd = join(base, 'project');
  const home = join(base, 'home');
  const config = join(base, 'config');
  const temp = join(base, 'tmp');
  const marker = `CIRCUS_NATIVE_${definition.id}`;
  const values = { CWD: cwd, HOME: home, CONFIG_DIR: config, TMPDIR: temp, CASE_ROOT: base };
  const steps = [];
  const notes = [];
  let index = 0;
  let pending;
  let version;
  let session;
  let tools;
  let fake;
  let sdk;
  let timer;
  let stderr = '';
  const abortController = new AbortController();
  const expand = value => replaceData(value, text => text.replace(/\{\{([A-Z_]+)\}\}/g, (match, key) => {
    if (!(key in values)) throw new Error(`Unresolved reference ${match}`);
    return values[key];
  }));

  function discover(content) {
    const text = typeof content === 'string' ? content : JSON.stringify(content);
    const task = text.match(/(?:running in background with ID:|task_id["\\:\s]+|task-id>)\s*([a-z0-9_-]+)/i);
    if (task) values.TASK_ID = task[1];
    const output = text.match(/(?:Output is being written to:|Full output saved to:|output_file["\\:\s]+)\s*([^\s"\\<>]+)/i)
      || text.match(/([^\s"\\<>]+\/tasks\/[a-z0-9_-]+\.output)/i);
    if (output) values.OUTPUT_PATH = output[1].replace(/\.(?=$)/, '');
    const snapshot = text.match(/([^\s"'\\;]+\/shell-snapshots\/snapshot-[^\s"'\\;]+\.sh)/);
    if (snapshot) values.SNAPSHOT_PATH = snapshot[1];
  }

  function normalizeString(text) {
    // Redact assignments in env output and shell snapshots, including the fake key.
    text = text.replace(/((?:[A-Za-z_][A-Za-z0-9_]*(?:TOKEN|SECRET|PASSWORD|CREDENTIAL|API_KEY|AUTHORIZATION)[A-Za-z0-9_]*|ANTHROPIC_API_KEY)=)(?:"[^"\n]*"|'[^'\n]*'|[^\s\n;]+)/gi, '$1<redacted>');
    text = text.replaceAll('sk-circus-fake-no-network', '<redacted>');
    for (const [key, value] of Object.entries(values).sort((a, b) => b[1].length - a[1].length)) {
      text = text.replaceAll(value, `{{${key}}}`);
    }
    if (session) text = text.replaceAll(session, '{{SESSION_ID}}');
    text = text.replaceAll(root, '{{CAPTURE_DIR}}');
    text = text.replaceAll(cwd.replaceAll('/', '-'), '{{PROJECT_KEY}}');
    text = text.replaceAll(tmpdir(), '{{SYSTEM_TMPDIR}}');
    text = text.replace(/127\.0\.0\.1:\d+/g, '127.0.0.1:{{PORT}}');
    text = text.replace(/\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b/gi, '{{SESSION_ID}}');
    text = text.replace(/snapshot-bash-\d+-[a-z0-9]+\.sh/g, 'snapshot-bash-{{TIME}}-{{ID}}.sh');
    text = text.replace(/cwd-[a-z0-9-]+/g, 'cwd-{{ID}}');
    text = text.replace(/(?<=\/tool-results\/)[^\s/"'<>]+/g, '{{ID}}.txt');
    text = text.replace(/(?<=\/tasks\/)[a-z0-9_-]+(?=\.output)/g, '{{TASK_ID}}');
    text = text.replace(/\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z\b/g, '{{TIME}}');
    text = text.replace(/(Wall time: )\d+(?:\.\d+)?( seconds?)/g, '$1{{DURATION}}$2');
    text = text.replace(/^[ \t]*\d+[ \t]+\d+[ \t]+(?=\/|\{\{CAPTURE_DIR\}\})/gm, '{{PID}} {{PID}} ');
    text = text.replace(/(\bps -o pid,ppid,args -p )\d+/g, '$1{{PID}}');
    text = text.replace(/(CLAUDE_PID=)\d+/g, '$1{{PID}}');
    text = text.replace(/(\/cc-socks\/)\d+(\.sock)/g, '$1{{PID}}$2');
    text = text.replace(/claude-[a-f0-9]+-cwd/g, 'claude-{{ID}}-cwd');
    return text;
  }
  const normalize = value => replaceData(value, normalizeString);

  try {
    for (const dir of [cwd, home, config, temp]) await mkdir(dir, { recursive: true });
    for (const dir of definition.setup.dirs) await mkdir(join(cwd, dir), { recursive: true });
    for (const [path, content] of Object.entries(definition.setup.files)) {
      const full = join(cwd, path);
      await mkdir(resolve(full, '..'), { recursive: true });
      await writeFile(full, typeof content === 'string' ? content : Buffer.from(content.base64, 'base64'));
    }

    fake = await startFakeApi({ marker, model, onError: () => abortController.abort(), onTurn: async body => {
      tools ??= body.tools;
      if (pending) {
        const containing = body.messages.filter(m => m.role === 'user' && Array.isArray(m.content) &&
          m.content.some(b => b.type === 'tool_result' && b.tool_use_id === pending.id));
        if (containing.length !== 1) throw new Error(`Expected exactly one user message for ${pending.id}`);
        const message = containing[0];
        const result = message.content.find(b => b.type === 'tool_result' && b.tool_use_id === pending.id);
        discover(result.content);
        steps.push({ tool: pending.tool, input: pending.input,
          ...(pending.before ? { before: pending.before } : {}),
          result: { is_error: result.is_error ?? false, content: result.content },
          attachments: message.content.filter(b => b !== result),
        });
        await writeFile(join(root, '.tmp', `${definition.id}-steps.json`), json(normalize(steps)));
        pending = null;
      }
      while (index < definition.steps.length) {
        const step = definition.steps[index++];
        if (step.when === 'snapshot' && !values.SNAPSHOT_PATH) {
          notes.push('No snapshot path appeared in shell-info; the conditional cat step was skipped.');
          continue;
        }
        const expanded = expand(step);
        if (expanded.before?.write) {
          const { path, content } = expanded.before.write;
          // Ensure mtime-based checks observe a distinct external write even on coarse filesystems.
          const previous = await stat(path);
          await writeFile(path, content);
          if ((await stat(path)).mtimeMs === previous.mtimeMs) {
            throw new Error('External modification did not change mtime');
          }
        }
        pending = { ...expanded, id: `toolu_native_${steps.length}` };
        return pending;
      }
      return { text: 'Native capture complete.' };
    } });

    // Options.env replaces the complete child environment. No host credentials,
    // provider overrides, shell startup files, proxies or Claude settings survive.
    const env = {
      PATH: '/usr/local/bin:/usr/bin:/bin', HOME: home, USER: 'capture', LOGNAME: 'capture',
      SHELL: '/bin/bash', LANG: 'C.UTF-8', TERM: 'dumb', TMPDIR: temp,
      ANTHROPIC_BASE_URL: fake.url, ANTHROPIC_API_KEY: 'sk-circus-fake-no-network',
      CLAUDE_CONFIG_DIR: config, DISABLE_TELEMETRY: '1', CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1',
    };
    timer = setTimeout(() => abortController.abort(), 60000);
    sdk = query({ prompt: `${marker}: Execute the supplied tool script and finish.`, options: {
      cwd, env, model, tools: toolNames, settingSources: [], mcpServers: {}, strictMcpConfig: true,
      permissionMode: 'bypassPermissions', allowDangerouslySkipPermissions: true, abortController,
      stderr: chunk => { stderr += chunk; },
    } });
    let final;
    for await (const message of sdk) {
      if (message.type === 'system' && message.subtype === 'init') {
        version = message.claude_code_version;
        session = message.session_id;
        if (message.apiKeySource !== 'ANTHROPIC_API_KEY') throw new Error('Unexpected authentication source');
        if (toolNames.some(t => !message.tools.includes(t))) throw new Error('A required native tool was not enabled');
      }
      if (message.type === 'result') final = message;
    }
    fake.check();
    if (final?.subtype !== 'success' || final.result !== 'Native capture complete.' || pending || !steps.length) {
      throw new Error(`Incomplete capture (${final?.subtype ?? 'no final result'})`);
    }
    if (!version || !tools) throw new Error('Missing CLI version or tool schemas');
    const filesAfter = {};
    async function walk(dir) {
      for (const entry of (await readdir(dir, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name))) {
        const path = join(dir, entry.name);
        if (entry.isDirectory()) await walk(path);
        else if (entry.isFile()) {
          const bytes = await readFile(path);
          const text = bytes.toString('utf8');
          filesAfter[relative(cwd, path)] = !bytes.includes(0) && Buffer.from(text).equals(bytes)
            ? text : { base64: bytes.toString('base64') };
        }
      }
    }
    await walk(cwd);
    const fixture = normalize({
      meta: { claude_code_version: version, sdk_version: '0.3.319', captured_at: '{{TIME}}' },
      case: definition.id, description: definition.description, setup: definition.setup, steps,
      files_after: filesAfter, ...(notes.length ? { notes } : {}),
    });
    const category = definition.id.startsWith('taskstop-') ? 'TaskStop' : definition.id.split('-')[0].replace(/^./, s => s.toUpperCase());
    const path = join(fixtures, category, `${definition.id}.json`);
    await mkdir(resolve(path, '..'), { recursive: true });
    const output = json(fixture);
    let previous;
    try { previous = await readFile(path, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    await writeFile(path, output);
    const schemas = json(normalize(tools));
    const toolsPath = join(fixtures, 'tools.json');
    let oldTools;
    try { oldTools = await readFile(toolsPath, 'utf8'); } catch (error) { if (error.code !== 'ENOENT') throw error; }
    if (oldTools && oldTools !== schemas) throw new Error('Tool schemas differ across captures');
    await writeFile(toolsPath, schemas);
    return { case: definition.id, sha256: digest(output), comparison: previous === undefined ? 'new' : previous === output ? 'identical' : 'changed', steps: steps.length, notes };
  } catch (error) {
    try { fake?.check(); } catch (fakeError) { error = fakeError; }
    // Diagnostics stay under tests/parity and are redacted before persistence.
    await writeFile(join(root, '.tmp', `${definition.id}-failure.txt`), normalizeString(`${error.stack}\n${stderr}`));
    throw error;
  } finally {
    clearTimeout(timer);
    try {
      // Query.close() starts asynchronous teardown but returns void. Returning
      // the generator awaits SDK teardown before deleting files it may flush.
      await sdk?.return();
    } finally {
      await fake?.close();
      await rm(base, { recursive: true, force: true, maxRetries: 5 });
    }
  }
}

const results = [];
for (const definition of selected) {
  try {
    const result = await capture(definition);
    results.push(result);
    console.log(`PASS ${definition.id}: ${result.steps} step(s), ${result.comparison}`);
  } catch (error) {
    results.push({ case: definition.id, error: error.message });
    console.error(`FAIL ${definition.id}: ${error.message}`);
  }
}
await writeFile(join(root, '.tmp', 'last-run.json'), json(results));
console.log(`Captured ${results.filter(r => !r.error).length}/${selected.length} cases.`);
if (results.some(r => r.error)) process.exitCode = 1;
