const bash = (command, extra = {}) => ({ tool: 'Bash', input: { command, description: 'Observe ampersand child lifetime', ...extra } });
const amp = (name, command, extra = {}, steps = []) => ({
  id: `bash-amp-child-${name}`, description: `Observe child and CLI exit: ${command}.`,
  setup: { files: {}, dirs: [] }, steps: [bash(command, { timeout: 10000, ...extra }), ...steps],
  observe_native: { processes: true }, deadline_ms: 60000,
});
export const ampCases = [
  amp('primary', 'sleep 20 & echo x', {}, [
    bash('ps -o pid,pgid,sid,ppid,stat,args -C sleep'),
    { ...bash('true'), before: { delay_ms: 22000 } },
  ]),
  amp('exit', 'sleep 20 & echo x'),
  amp('managed', 'sleep 20 & echo x', { run_in_background: true }),
  amp('double-fork', '(sleep 20 &); echo x'),
  amp('nohup', 'nohup sleep 20 >/dev/null 2>&1 & echo x'),
  amp('stdout-null', 'sleep 20 > /dev/null & echo x'),
  amp('setsid', 'setsid sleep 20 & echo x'),
  amp('marker', "bash -c 'exec -a circus_probe_bash_amp_child_marker sleep 20' & echo x"),
  amp('timeout', 'sleep 20 & sleep 3; echo x', { timeout: 1000 }),
  amp('term-trap', 'bash -c \'trap "echo TERM >> {{CWD}}/sig" TERM; sleep 20\' & echo x'),
];

export function hugeCase(name, size, input, prefix = '') {
  return { id: `read-huge-line-${name}`, description: `Read ${size} generated x bytes${prefix ? ' after a short first line' : ''}.`,
    setup: { files: {}, dirs: [], generate: { path: 'huge.txt', byte: 'x', size, newline: true, ...(prefix ? { prefix } : {}) } },
    steps: [{ tool: 'Read', input: { file_path: '{{CWD}}/huge.txt', ...input } }],
    observe_native: { processes: false }, summarize_large: true, deadline_ms: 180000,
  };
}
export const hugeCases = [
  hugeCase('300m-limit', 314572800, { limit: 1 }),
  hugeCase('300m-offset-limit', 314572800, { offset: 1, limit: 1 }),
  hugeCase('300m-default', 314572800, {}),
  hugeCase('two-300m-limit', 314572800, { limit: 1 }, '0123456789\n'),
  hugeCase('two-300m-offset-limit', 314572800, { offset: 1, limit: 1 }, '0123456789\n'),
  hugeCase('two-300m-default', 314572800, {}, '0123456789\n'),
  hugeCase('1m-limit', 1048576, { limit: 1 }),
  hugeCase('10m-limit', 10485760, { limit: 1 }),
  hugeCase('100m-limit', 104857600, { limit: 1 }),
];
// Keep representative native results; the measurement script retains all
// requested variants and all bisection probes as documented evidence.
export const ampHugeCases = [
  ...ampCases.filter(c => ['primary', 'exit', 'managed', 'timeout', 'term-trap'].some(name => c.id === `bash-amp-child-${name}`)),
  ...hugeCases.filter(c => ['300m-limit', '300m-default', 'two-300m-limit', '1m-limit'].some(name => c.id === `read-huge-line-${name}`)),
  hugeCase('boundary-inline', 3200000, { limit: 1 }),
  hugeCase('boundary-over', 3200001, { limit: 1 }),
];
