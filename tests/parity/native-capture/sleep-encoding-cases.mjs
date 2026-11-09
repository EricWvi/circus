export const bash = (command, extra = {}) => ({ tool: 'Bash', input: {
  command, description: 'Measure sleep guard and timeout behavior', ...extra,
} });
export const stop = { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' }, when: 'task' };
export const shellCase = (id, command, options = {}, steps = [stop]) => ({
  id, description: `Measure native Bash: ${command}.`, setup: { files: {}, dirs: [] },
  steps: [bash(command, options), ...steps],
});

const read = { tool: 'Read', input: { file_path: '{{CWD}}/a.txt' } };
const legacy = Buffer.from([0x61, 0xff, 0x62, 0xfe, 0xfd, 0x63, 0x0a]);
const utf16 = Buffer.from([0xff, 0xfe, 0x68, 0, 0x69, 0, 0x0a, 0]);
export const encodingCases = ['legacy', 'utf16le'].flatMap(encoding => ['Edit', 'Write'].flatMap(tool => [false, true].map(afterRead => ({
  id: `${tool.toLowerCase()}-invalid-utf8-${encoding}${afterRead ? '-after-read' : ''}`,
  description: `${tool} ${encoding} bytes ${afterRead ? 'after Read' : 'without a prior Read'}.`,
  setup: { files: { 'a.txt': { base64: (encoding === 'legacy' ? legacy : utf16).toString('base64') } }, dirs: [] },
  steps: [
    ...(afterRead ? [read] : []),
    tool === 'Edit' ? { tool, input: { file_path: '{{CWD}}/a.txt', old_string: encoding === 'legacy' ? 'a' : 'hi', new_string: encoding === 'legacy' ? 'A' : 'hello' } }
      : { tool, input: { file_path: '{{CWD}}/a.txt', content: 'replacement\n' } },
  ],
}))));

// Retain representatives, not every bisection/shape probe. Existing fixtures
// already pin a bare sleep timeout and stdout-before-timeout behavior.
export const sleepEncodingCases = [
  shellCase('bash-sleep-guard-standalone-24', 'sleep 24', { timeout: 1000 }),
  shellCase('bash-sleep-guard-standalone-25', 'sleep 25', { timeout: 1000 }),
  shellCase('bash-sleep-guard-leading-25', 'sleep 25; echo x', { timeout: 1000 }),
  shellCase('bash-sleep-guard-pipeline-30', 'sleep 30 | cat', { timeout: 1000 }),
  shellCase('bash-sleep-guard-unit-minute', 'sleep 1m', { timeout: 1000 }),
  shellCase('bash-sleep-guard-background-leading', 'sleep 30; echo x', { run_in_background: true }),
  shellCase('bash-timeout-bg-no-output', 'true; sleep 5', { timeout: 1000 }),
  shellCase('bash-timeout-bg-no-newline', 'printf x; sleep 5', { timeout: 1000 }),
  shellCase('bash-timeout-bg-leading-output-killed', 'sleep 0.1; echo x; sleep 5', { timeout: 1000 }),
  shellCase('bash-timeout-bg-two-seconds', 'echo before; sleep 5', { timeout: 2000 }),
  shellCase('bash-timeout-bg-lifecycle', 'echo before; sleep 5', { timeout: 1000 }, [
    bash('sleep 6', { timeout: 10000 }),
    { tool: 'Read', input: { file_path: '{{OUTPUT_PATH}}' } },
    { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' } },
  ]),
  ...encodingCases.filter(c => c.id.includes('-legacy') || c.id === 'edit-invalid-utf8-utf16le-after-read' || c.id === 'write-invalid-utf8-utf16le'),
];
