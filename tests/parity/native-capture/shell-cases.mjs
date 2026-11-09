import { existsSync } from 'node:fs';

const bash = command => ({ tool: 'Bash', input: { command, description: 'Inspect shell profile and snapshot behavior' } });
const steps = [
  bash('env | grep MARK_ | sort; echo "local=[$LOCAL_BASHRC]"; echo "$PATH" | tr : \'\\n\' | head -3'),
  bash('ll_mark; fn_mark; type ll_mark fn_mark 2>&1'),
  { ...bash('cat "{{SNAPSHOT_PATH}}"'), resolve_snapshot: true },
  bash('export SESSION_VAR=1; FOO_SHELL=1'),
  bash('echo "[$SESSION_VAR][$FOO_SHELL]"'),
  bash('cd does-not-exist; pwd'),
  bash('pwd'),
];

export const shellCases = [{
  id: 'bash-profile-sourcing', description: 'Measure Bash startup-file sourcing and snapshot state.',
  setup: { files: {}, dirs: [], home_files: {
    '.bashrc': 'export MARK_BASHRC=1\nLOCAL_BASHRC=1\nalias ll_mark=\'echo alias-ok\'\nfn_mark() { echo fn-ok; }\nexport PATH="$HOME/bin-bashrc:$PATH"\nexport MARK_BASHRC_FLAGS="$-"\ncase $- in *i*) export MARK_INTERACTIVE=1;; esac\n',
    // Aliases are serialized into the snapshot even when exported markers are
    // not transferred. They distinguish "not sourced" from "not propagated".
    '.bash_profile': 'export MARK_BASH_PROFILE=1\nalias source_bash_profile_mark=\'echo .bash_profile\'\nalias snapshot_flags_mark="echo flags=$-"\n',
    '.profile': 'export MARK_PROFILE=1\nalias source_profile_mark=\'echo .profile\'\n',
    '.bash_login': 'export MARK_BASH_LOGIN=1\nalias source_bash_login_mark=\'echo .bash_login\'\n',
  } }, steps,
}];

if (existsSync('/bin/zsh')) shellCases.push({
  id: 'bash-profile-sourcing-zsh', description: 'Measure zsh startup-file sourcing and snapshot state.', shell: '/bin/zsh',
  setup: { files: {}, dirs: [], home_files: {
    '.zshrc': 'export MARK_ZSHRC=1\nLOCAL_BASHRC=1\nalias ll_mark=\'echo alias-ok\'\nfn_mark() { echo fn-ok; }\nexport PATH="$HOME/bin-zshrc:$PATH"\nexport MARK_ZSHRC_FLAGS="$-"\ncase $- in *i*) export MARK_INTERACTIVE=1;; esac\n',
    '.zprofile': 'export MARK_ZPROFILE=1\n',
    '.zshenv': 'export MARK_ZSHENV=1\n',
  } }, steps,
});
