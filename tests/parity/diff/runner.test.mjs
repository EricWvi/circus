import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { generate } from './generate.mjs';
import { canonical, extractSessions, repo, sessionCase } from './extract-sessions.mjs';

test('seeded coverage is reproducible, bounded, and spans the required families', () => {
  const cases = generate({ mode: 'deck', seed: 1, count: 140 });
  assert.equal(canonical(cases), canonical(generate({ mode: 'deck', seed: 1, count: 140 })));
  assert.notEqual(canonical(cases), canonical(generate({ mode: 'deck', seed: 2, count: 140 })));
  assert.equal(new Set(cases.map(c => c.id)).size, 140);
  assert.ok(cases.every(c => c.steps.length >= 1 && c.steps.length <= 4));
  const kinds = cases.map(c => c.id.replace(/^gen-1-\d+-/, ''));
  for (const kind of ['quotes', 'heredoc', 'herestring', 'invalid-utf8', 'nul', 'cwd', 'export', 'timeout', 'background-stop', 'read-size-262145', 'write-readonly', 'edit-unread', 'edit-curly']) assert.ok(kinds.includes(kind), kind);
  assert.throws(() => generate({ count: -1 }));
});

test('combo generation varies independent commands and stateful tool sequences', () => {
  const cases = generate({ mode: 'combo', seed: 1, count: 200 });
  assert.equal(canonical(cases), canonical(generate({ mode: 'combo', seed: 1, count: 200 })));
  assert.notEqual(canonical(cases), canonical(generate({ mode: 'combo', seed: 2, count: 200 })));
  assert.equal(cases[0].id, 'combo-1-1');
  assert.equal(cases.at(-1).id, 'combo-1-200');
  assert.ok(cases.every(c => c.steps.length >= 1 && c.steps.length <= 4));
  assert.ok(new Set(cases.map(c => canonical({ setup: c.setup, steps: c.steps }))).size > 190);
  assert.deepEqual(new Set(cases.flatMap(c => c.steps.map(s => s.tool))), new Set(['Bash', 'Read', 'Write', 'Edit']));
  assert.deepEqual(new Set(cases.map(c => c.steps.length)), new Set([1, 2, 3, 4]));
  assert.ok(cases.some(c => c.steps.some((step, i) => step.tool === 'Edit' && c.steps[i - 1]?.tool === 'Read')));
  assert.ok(cases.some(c => c.steps[0].tool === 'Edit'));
  for (const definition of cases) for (const step of definition.steps) if (step.tool === 'Bash') {
    assert.ok(!step.input.command.includes('\0'), definition.id);
    const result = spawnSync('/bin/bash', ['-n', '-c', step.input.command], { encoding: 'utf8' });
    assert.equal(result.status, 0, `${definition.id}: ${result.stderr}`);
  }
  assert.throws(() => generate({ mode: 'unknown' }));
});

test('extractor retains only deduplicated assistant tool inputs and enforces the byte limit', async () => {
  await mkdir(join(repo, '.tmp'), { recursive: true });
  const base = await mkdtemp(join(repo, '.tmp/diff-extractor-test-'));
  try {
    const source = join(base, 'projects/nested');
    const output = join(base, 'corpus.jsonl');
    await mkdir(source, { recursive: true });
    const tool = { type: 'tool_use', name: 'Read', input: { file_path: '/private/example.txt', offset: 1 } };
    const message = content => JSON.stringify({ message: { role: 'assistant', content } });
    await writeFile(join(source, 'session.jsonl'), [
      message([{ type: 'text', text: 'private conversation' }, tool, { ...tool, input: { offset: 1, file_path: '/private/example.txt' } }]),
      JSON.stringify({ message: { role: 'user', content: [tool, { type: 'tool_result', content: 'private output' }] } }),
      message([{ type: 'tool_use', name: 'Other', input: { text: 'ignored' } }, { type: 'tool_use', name: 'Write', input: { file_path: 'x', content: '中'.repeat(30000) } }]),
      '{invalid',
    ].join('\n'));
    assert.deepEqual(await extractSessions({ source: join(base, 'projects'), output }), { inputs: 1, malformed: 1 });
    const text = await readFile(output, 'utf8');
    assert.deepEqual(JSON.parse(text), { tool: 'Read', input: tool.input });
    assert.ok(!text.includes('private conversation'));
    assert.ok(!text.includes('private output'));
  } finally { await rm(base, { recursive: true, force: true }); }
});

test('session files stay in the project and Edit is bootstrapped without transcript output', () => {
  const definition = sessionCase({ tool: 'Edit', input: { file_path: '/private/path/a.txt', old_string: 'old', new_string: 'new' } }, 0);
  assert.deepEqual(definition.steps.map(s => s.tool), ['Write', 'Edit']);
  assert.ok(definition.steps.every(s => s.input.file_path === '{{CWD}}/a.txt'));
  assert.equal(definition.steps[0].input.content, 'old');
  assert.equal(sessionCase({ tool: 'Read', input: { file_path: '../../etc/passwd' } }, 1).steps[0].input.file_path, '{{CWD}}/passwd');
  assert.equal(sessionCase({ tool: 'Read', input: { file_path: 'nested/a.txt' } }, 3).steps[0].input.file_path, 'nested/a.txt');
  const command = 'printf safe';
  assert.equal(sessionCase({ tool: 'Bash', input: { command } }, 2).steps[0].input.command, command);
});
