import { shellCases } from './shell-cases.mjs';
import { thresholdCases } from './threshold-cases.mjs';
import { metadataCases } from './metadata-cases.mjs';

const text = 'alpha\nbeta\ngamma\ndelta\nepsilon\n';
const read = (file_path = '{{CWD}}/a.txt', extra = {}) => ({ tool: 'Read', input: { file_path, ...extra } });
const write = (file_path = '{{CWD}}/a.txt', content = 'replacement\n') => ({ tool: 'Write', input: { file_path, content } });
const edit = (old_string = 'beta', new_string = 'BETA', extra = {}) => ({ tool: 'Edit', input: { file_path: '{{CWD}}/a.txt', old_string, new_string, ...extra } });
const bash = (command, extra = {}) => ({ tool: 'Bash', input: { command, description: 'Capture native Bash behavior', ...extra } });
const c = (id, description, files, steps, dirs = []) => ({ id, description, setup: { files, dirs }, steps });

// Definitions are plain data. Runtime references are resolved from native results.
export const cases = [
  c('read-basic', 'Read five lines with a trailing newline.', { 'a.txt': text }, [read()]),
  c('read-no-trailing-newline', 'Read five lines without a trailing newline.', { 'a.txt': text.trimEnd() }, [read()]),
  c('read-crlf', 'Read CRLF line endings.', { 'a.txt': text.replaceAll('\n', '\r\n') }, [read()]),
  c('read-unicode', 'Read CJK and emoji.', { 'a.txt': '你好，世界\nClaude 🐱 🚀\n' }, [read()]),
  c('read-tabs', 'Read literal tabs.', { 'a.txt': '\talpha\nbeta\tgamma\n\t\tdelta\n' }, [read()]),
  c('read-offset-limit', 'Read five lines starting at line ten.', { 'a.txt': Array.from({ length: 50 }, (_, i) => `line ${i + 1}\n`).join('') }, [read(undefined, { offset: 10, limit: 5 })]),
  c('read-offset-past-end', 'Read beyond EOF.', { 'a.txt': text }, [read(undefined, { offset: 100 })]),
  c('read-long-file', 'Read 3000 lines with default limits.', { 'a.txt': Array.from({ length: 3000 }, (_, i) => `line ${i + 1}\n`).join('') }, [read()]),
  c('read-long-line', 'Read one 5000-character line.', { 'a.txt': 'x'.repeat(5000) }, [read()]),
  c('read-empty', 'Read an empty file.', { 'a.txt': '' }, [read()]),
  c('read-missing', 'Read a nonexistent file.', {}, [read()]),
  c('read-directory', 'Read a directory.', {}, [read('{{CWD}}/sub')], ['sub']),
  c('read-binary', 'Read a .bin file containing NUL bytes.', { 'a.bin': { base64: Buffer.from([0, 1, 2, 0, 255, 10]).toString('base64') } }, [read('{{CWD}}/a.bin')]),
  c('read-relative-path', 'Read a relative path.', { 'a.txt': text }, [read('a.txt')]),
  c('read-twice-unchanged', 'Read the same unchanged file twice.', { 'a.txt': text }, [read(), read()]),
  c('read-twice-changed', 'Read again after an external modification.', { 'a.txt': text }, [read(), { ...read(), before: { write: { path: '{{CWD}}/a.txt', content: 'external change\n' } } }]),
  c('write-new', 'Create a file in an existing directory.', {}, [write()]),
  c('write-new-nested', 'Create a file and missing parent directories.', {}, [write('{{CWD}}/one/two/a.txt')]),
  c('write-existing-never-read', 'Overwrite an existing file without reading it.', { 'a.txt': text }, [write()]),
  c('write-after-read', 'Overwrite a previously read file.', { 'a.txt': text }, [read(), write()]),
  c('write-after-read-modified', 'Write after an external modification since Read.', { 'a.txt': text }, [read(), { ...write(), before: { write: { path: '{{CWD}}/a.txt', content: 'external change\n' } } }]),
  c('write-relative-path', 'Write a relative path.', {}, [write('a.txt')]),
  c('edit-basic', 'Replace one unique string after Read.', { 'a.txt': text }, [read(), edit()]),
  c('edit-never-read', 'Edit a file without reading it.', { 'a.txt': text }, [edit()]),
  c('edit-not-found', 'Replace a string absent from a read file.', { 'a.txt': text }, [read(), edit('absent')]),
  c('edit-multiple-matches', 'Replace an ambiguous string without replace_all.', { 'a.txt': 'beta\nbeta\n' }, [read(), edit()]),
  c('edit-replace-all', 'Replace every occurrence.', { 'a.txt': 'beta\nbeta\n' }, [read(), edit('beta', 'BETA', { replace_all: true })]),
  c('edit-same-strings', 'Use identical old_string and new_string.', { 'a.txt': text }, [read(), edit('beta', 'beta')]),
  c('edit-missing-file', 'Edit a nonexistent file with nonempty old_string.', {}, [edit()]),
  c('edit-create-with-empty-old', 'Create a nonexistent file using empty old_string.', {}, [edit('', 'created\n')]),
  c('edit-after-read-modified', 'Edit a unique string after an unrelated external modification.', { 'a.txt': text }, [read(), { ...edit(), before: { write: { path: '{{CWD}}/a.txt', content: text.replace('epsilon', 'external') } } }]),
  c('edit-after-read-modified-conflict', 'Edit after external modification removes the target.', { 'a.txt': text }, [read(), { ...edit(), before: { write: { path: '{{CWD}}/a.txt', content: text.replace('beta', 'external') } } }]),
  c('edit-crlf', 'Edit a CRLF file and preserve resulting bytes.', { 'a.txt': text.replaceAll('\n', '\r\n') }, [read(), edit()]),
  c('edit-curly-quotes', 'Match straight quotes against curly quotes.', { 'a.txt': 'before “quoted” after\n' }, [read(), edit('"quoted"', '"replaced"')]),
  c('edit-multiline', 'Replace a string spanning three lines.', { 'a.txt': text }, [read(), edit('beta\ngamma\ndelta', 'BETA\nGAMMA\nDELTA')]),
  c('bash-echo', 'Echo a line.', {}, [bash('echo hello')]),
  c('bash-empty-output', 'Run true without output.', {}, [bash('true')]),
  c('bash-exit-code', 'Exit with status three.', {}, [bash('exit 3')]),
  c('bash-stderr', 'Write stdout and stderr.', {}, [bash('echo out; echo err >&2')]),
  c('bash-stderr-and-fail', 'Write stderr and exit one.', {}, [bash('echo err >&2; exit 1')]),
  c('bash-cd-persists', 'Change into a project subdirectory, then inspect cwd.', {}, [bash('cd sub'), bash('pwd')], ['sub']),
  c('bash-cd-outside', 'Change outside the project, then inspect cwd.', {}, [bash('cd /'), bash('pwd')]),
  c('bash-timeout', 'Sleep five seconds with a one-second timeout.', {}, [bash('sleep 5', { timeout: 1000 })]),
  c('bash-large-output', 'Produce 100000 numbered lines.', {}, [bash('seq 1 100000')]),
  c('bash-long-single-line', 'Produce one 200000-character line.', {}, [bash("head -c 200000 /dev/zero | tr '\\0' a")]),
  c('bash-stdin', 'Read from closed stdin.', {}, [bash('read x; echo "[$x]"')]),
  c('bash-env', 'Record sorted environment in an isolated, credential-free shell.', {}, [bash('env | sort')]),
  c('bash-shell-info', 'Inspect shell flags, wrapper, shopt, aliases and referenced snapshot.', {}, [
    bash('echo "$0|$SHELL|$-"; ps -o pid,ppid,args -p $$; ps -o pid,ppid,args -p $PPID; shopt -p; alias'),
    { ...bash('cat "{{SNAPSHOT_PATH}}"'), when: 'snapshot' },
  ]),
  c('bash-background', 'Run a background loop, wait, then read its actual output file.', {}, [
    bash('for i in 1 2 3; do echo tick $i; sleep 1; done', { run_in_background: true }),
    bash('sleep 4'), read('{{OUTPUT_PATH}}'),
  ]),
  c('bash-background-stop', 'Stop a background sleep using its actual task id.', {}, [
    bash('sleep 60', { run_in_background: true }), { tool: 'TaskStop', input: { task_id: '{{TASK_ID}}' } },
  ]),
  c('taskstop-unknown', 'Stop a nonexistent task.', {}, [{ tool: 'TaskStop', input: { task_id: 'bogus-task-id' } }]),
  ...shellCases,
  ...thresholdCases,
  ...metadataCases,
];
