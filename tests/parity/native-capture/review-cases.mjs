const bash = (command, extra = {}) => ({ tool: 'Bash', input: { command, description: 'Capture M1 review behavior', ...extra } });
const read = (file_path, extra = {}) => ({ tool: 'Read', input: { file_path, ...extra } });
const edit = (old_string, new_string) => ({ tool: 'Edit', input: { file_path: '{{CWD}}/a.txt', old_string, new_string } });
const c = (id, description, files, steps, extra = {}) => ({ id, description, setup: { files, dirs: [] }, steps, ...extra });
// Exactly 262145 bytes: 24840 complete ASCII lines (262134 bytes), then 11 bytes.
const aboveLimit = Array.from({ length: 24841 }, (_, i) => `line ${i + 1}\n`).join('');
const invalidUtf8 = { base64: Buffer.from([0x61, 0xff, 0x62, 0xfe, 0xfd, 0x63, 0x0a]).toString('base64') };

export const reviewCases = [
  c('bash-background-child-foreground', 'Measure foreground shell exit with a background child retaining output pipes.', {}, [
    bash('sleep 30 & echo started', { timeout: 10000 }),
    bash("ps -o pid,args -C sleep | grep 'sleep 30' || echo gone"),
  ]),
  c('bash-timeout-with-output', 'Keep output emitted before a foreground timeout.', {}, [
    bash('echo before; sleep 5', { timeout: 1000 }),
  ]),
  c('bash-timeout-long', 'Measure timeout wording above one minute.', {}, [bash('sleep 70', { timeout: 65000 })],
    { slow: true, deadline_ms: 90000 }),
  c('bash-invalid-utf8', 'Decode invalid UTF-8 bytes from Bash output.', {}, [bash("printf 'a\\xffb\\xfe\\xfdc\\n'")]),
  c('bash-cwd-deleted', 'Remove the persisted shell working directory before subsequent commands.', {}, [
    bash('mkdir gone && cd gone'),
    { ...bash('pwd'), before: { remove: { path: '{{CWD}}/gone' } } },
    bash('pwd'),
  ]),
  c('bash-preview-astral', 'Measure preview length for 35000 bytes of four-byte emoji.', {}, [
    bash("printf '%s' \"$(head -c 8750 /dev/zero | tr '\\0' a)\" | sed 's/a/😀/g'"),
  ]),
  c('taskstop-finished', 'Stop a background task after it has completed.', {}, [
    bash('echo done', { run_in_background: true }), bash('sleep 2'),
    { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' } },
  ]),
  c('read-threshold-offset-only', 'Read a 262145-byte ASCII file with offset but no limit.', { 'a.txt': aboveLimit }, [
    read('{{CWD}}/a.txt', { offset: 1 }),
  ]),
  c('read-threshold-limit-only', 'Read a 262145-byte ASCII file with a large explicit limit but no offset.', { 'a.txt': aboveLimit }, [
    read('{{CWD}}/a.txt', { limit: 100000 }),
  ]),
  c('read-invalid-utf8', 'Read invalid UTF-8 bytes without NUL.', { 'a.txt': invalidUtf8 }, [read('{{CWD}}/a.txt')]),
  c('read-fifo', 'Read an unwritten FIFO with a ten-second harness guard.', {}, [
    { ...read('{{CWD}}/p'), guard_ms: 10000 },
  ], { setup: { files: {}, dirs: [], fifos: ['p'] } }),
  c('read-dev-null', 'Read the null character device.', {}, [read('/dev/null')]),
  c('edit-curly-single-quote', 'Match a straight apostrophe against a curly apostrophe.', { 'a.txt': 'don’t\n' }, [
    read('{{CWD}}/a.txt'), edit("don't", 'do not'),
  ]),
  c('edit-curly-single-quote-restyle', 'Preserve curly apostrophe style in a straight-quoted replacement.', { 'a.txt': 'don’t\n' }, [
    read('{{CWD}}/a.txt'), edit("don't", "won't"),
  ]),
  c('edit-mixed-eol', 'Replace across mixed CRLF and LF line endings.', { 'a.txt': 'a\r\nb\nc\r\n' }, [
    read('{{CWD}}/a.txt'), edit('b\nc', 'B\nC'),
  ]),
];
