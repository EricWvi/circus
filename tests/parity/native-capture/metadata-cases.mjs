const text = 'alpha\nbeta\ngamma\n';
const read = file => ({ tool: 'Read', input: { file_path: `{{CWD}}/${file}` } });
const write = (file, content = 'replacement\n') => ({ tool: 'Write', input: { file_path: `{{CWD}}/${file}`, content } });
const edit = (file, old_string = 'beta', new_string = 'BETA') => ({ tool: 'Edit', input: { file_path: `{{CWD}}/${file}`, old_string, new_string } });
const c = (id, description, setup, steps, observe_fs) => ({ id, description,
  setup: { files: {}, dirs: [], ...setup }, steps, observe_fs });

export const metadataCases = [
  c('write-through-symlink', 'Write a relative symlink and observe the link and target.',
    { files: { 'real.txt': text }, symlinks: { 'link.txt': 'real.txt' } },
    [write('link.txt')], ['link.txt', 'real.txt']),
  c('edit-through-symlink', 'Read then edit a relative symlink and observe the link and target.',
    { files: { 'real.txt': text }, symlinks: { 'link.txt': 'real.txt' } },
    [read('link.txt'), edit('link.txt')], ['link.txt', 'real.txt']),
  c('write-dangling-symlink', 'Write a dangling symlink and observe whether its target is created.',
    { symlinks: { 'link.txt': 'missing.txt' } }, [write('link.txt')], ['link.txt', 'missing.txt']),
  c('write-hardlink', 'Write one hard-linked name and compare both final inodes and contents.',
    { files: { 'a.txt': text }, hardlinks: { 'b.txt': 'a.txt' } }, [write('a.txt')], ['a.txt', 'b.txt']),
  c('edit-hardlink', 'Read then edit one hard-linked name and compare both final inodes and contents.',
    { files: { 'a.txt': text }, hardlinks: { 'b.txt': 'a.txt' } }, [read('a.txt'), edit('a.txt')], ['a.txt', 'b.txt']),
  c('write-preserves-mode', 'Write an existing executable script and observe its permissions.',
    { files: { 'script.sh': '#!/bin/sh\necho before\n' }, modes: { 'script.sh': '0755' } },
    [write('script.sh', '#!/bin/sh\necho after\n')], ['script.sh']),
  c('edit-preserves-mode', 'Read then edit an executable script and observe its permissions.',
    { files: { 'script.sh': '#!/bin/sh\necho before\n' }, modes: { 'script.sh': '0755' } },
    [read('script.sh'), edit('script.sh', 'before', 'after')], ['script.sh']),
  c('write-new-mode', 'Create flat and nested files under an explicit 022 umask.', {},
    [write('new.txt'), write('d1/d2/new.txt')], ['new.txt', 'd1', 'd1/d2', 'd1/d2/new.txt']),
  c('edit-empty-old-existing', 'Use empty old_string on a nonempty existing file without Read.',
    { files: { 'a.txt': text } }, [edit('a.txt', '', 'replacement\n')], ['a.txt']),
  c('edit-empty-old-existing-after-read', 'Use empty old_string on a nonempty existing file after Read.',
    { files: { 'a.txt': text } }, [read('a.txt'), edit('a.txt', '', 'replacement\n')], ['a.txt']),
  c('edit-empty-old-existing-empty', 'Use empty old_string on an existing empty file without Read.',
    { files: { 'a.txt': '' } }, [edit('a.txt', '', 'replacement\n')], ['a.txt']),
  c('read-symlink', 'Read through a relative symlink and observe link and target metadata.',
    { files: { 'real.txt': text }, symlinks: { 'link.txt': 'real.txt' } },
    [read('link.txt')], ['link.txt', 'real.txt']),
];
