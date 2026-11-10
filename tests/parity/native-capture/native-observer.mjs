import { readFile, readlink } from 'node:fs/promises';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';

const run = promisify(execFile);
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function proc(pid) {
  try {
    const raw = await readFile(`/proc/${pid}/stat`, 'utf8');
    const end = raw.lastIndexOf(')');
    const fields = raw.slice(end + 2).split(' ');
    return { pid: Number(pid), comm: raw.slice(raw.indexOf('(') + 1, end), stat: fields[0],
      ppid: Number(fields[1]), pgid: Number(fields[2]), sid: Number(fields[3]), start: fields[19],
      exit_status: Number(fields[49]) };
  } catch { return null; }
}

export class NativeObserver {
  constructor(cwd, processes) {
    this.cwd = cwd;
    this.processes = processes;
    this.owned = new Map();
    this.parents = new Map();
    this.labels = new Map([[process.pid, '{{HARNESS_PID}}']]);
    this.data = { first_seen: [], snapshots: [], max_vm_hwm_kib: 0, cleanup_signals: [] };
    this.pending = Promise.resolve();
  }
  attach(child) {
    this.child = child;
    this.labels.set(child.pid, '{{CLI_PID}}');
    this.exited = new Promise(resolve => child.once('exit', (code, signal) => {
      this.data.cli_exit = { code, signal }; resolve();
    }));
    this.interval = setInterval(() => {
      this.pending = this.pending.then(() => this.sample()).catch(() => {});
    }, 20);
  }
  label(pid, preferred) {
    if (!this.labels.has(pid)) this.labels.set(pid, preferred ?? `{{PROCESS_${this.labels.size}_PID}}`);
    return this.labels.get(pid);
  }
  async sample() {
    if (!this.child || this.data.cli_exit) return;
    try {
      const status = await readFile(`/proc/${this.child.pid}/status`, 'utf8');
      this.data.max_vm_hwm_kib = Math.max(this.data.max_vm_hwm_kib, Number(status.match(/^VmHWM:\s+(\d+)/m)?.[1] ?? 0));
    } catch { /* The process can exit between samples. */ }
    if (this.processes && performance.now() - (this.lastScan ?? 0) > 100) {
      this.lastScan = performance.now();
      await this.track();
    }
  }
  async track() {
    const { stdout } = await run('/usr/bin/ps', ['-o', 'pid,pgid,sid,ppid,stat,args', '-C', 'sleep']).catch(error => ({ stdout: error.stdout ?? '' }));
    for (const line of stdout.split('\n').slice(1)) {
      const pid = Number(line.trim().split(/\s+/)[0]);
      if (!pid || this.owned.has(pid)) continue;
      try {
        if (await readlink(`/proc/${pid}/cwd`) !== this.cwd) continue;
        const info = await proc(pid);
        if (info) {
          this.owned.set(pid, info.start);
          const args = (await readFile(`/proc/${pid}/cmdline`)).toString().replaceAll('\0', ' ').trim();
          this.label(pid, args.endsWith(' 20') ? '{{CHILD_PID}}' : '{{FOREGROUND_SLEEP_PID}}');
          const parent = await proc(info.ppid);
          const fd = {};
          for (const number of [1, 2]) fd[number] = await readlink(`/proc/${pid}/fd/${number}`).catch(error => ({ error: error.code }));
          this.data.first_seen.push({ ...info, args, parent, fd });
          if (parent?.comm === 'bash' && await readlink(`/proc/${parent.pid}/cwd`).catch(() => '') === this.cwd) {
            this.parents.set(parent.pid, parent.start);
            this.label(parent.pid, '{{SHELL_PID}}');
          }
        }
      } catch { /* Ignore unrelated or already exited processes. */ }
    }
    return stdout;
  }
  async snapshot(label) {
    await this.sample();
    if (!this.processes) return;
    const stdout = await this.track();
    const cli = this.child ? await proc(this.child.pid) : null;
    if (cli) { this.label(cli.pgid, '{{CLI_PGID}}'); this.label(cli.sid, '{{CLI_SID}}'); }
    if (cli) this.lastCli = cli;
    const referenceCli = cli ?? this.lastCli;
    const children = [];
    for (const [pid, start] of this.owned) {
      const info = await proc(pid);
      if (!info || info.start !== start) { children.push({ pid, exists: false }); continue; }
      const parent = await proc(info.ppid);
      this.label(info.ppid, info.ppid === 1 ? '{{INIT_PID}}' : undefined);
      this.label(info.pgid, '{{CHILD_PGID}}'); this.label(info.sid, '{{CHILD_SID}}');
      // Prefer relationship roles over a transient observation of the shell
      // leader while it is still alive. Discovery timing must not rename IDs.
      if (info.pgid !== info.pid && info.pgid !== referenceCli?.pgid) this.labels.set(info.pgid, '{{CHILD_PGID}}');
      if (info.sid !== info.pid && info.sid !== referenceCli?.sid) this.labels.set(info.sid, info.sid === info.pgid ? '{{CHILD_PGID}}' : '{{CHILD_SID}}');
      const fd = {};
      for (const number of [1, 2]) {
        try { fd[number] = await readlink(`/proc/${pid}/fd/${number}`); }
        catch (error) { fd[number] = { error: error.code }; }
      }
      const ls = await run('/usr/bin/ls', ['-l', `/proc/${pid}/fd/1`, `/proc/${pid}/fd/2`])
        .then(r => r.stdout).catch(error => error.stdout || error.stderr || error.code);
      children.push({ ...info, exists: true, args: (await readFile(`/proc/${pid}/cmdline`).catch(() => Buffer.alloc(0))).toString().replaceAll('\0', ' ').trim(),
        parent, fd, ls, parent_is_cli: info.ppid === this.child?.pid,
        pgid_is_cli_pgid: referenceCli ? info.pgid === referenceCli.pgid : null,
        sid_is_cli_sid: referenceCli ? info.sid === referenceCli.sid : null });
    }
    this.data.snapshots.push({ label, cli, ps: stdout, children });
  }
  normalize(text) {
    if (!this.processes) return text;
    const lines = text.split('\n');
    const header = lines.findIndex(line => /^[ \t]*PID[ \t]+PGID[ \t]+SID[ \t]+PPID[ \t]+STAT[ \t]+COMMAND$/.test(line));
    if (header >= 0) {
      let end = header + 1;
      while (/^[ \t]*\d+[ \t]+\d+[ \t]+\d+[ \t]+\d+[ \t]+/.test(lines[end] ?? '')) end++;
      const own = lines.slice(header + 1, end).filter(line => this.owned.has(Number(line.trim().split(/\s+/)[0])));
      lines.splice(header + 1, end - header - 1, '{{UNRELATED_SLEEP_ROWS}}', ...own);
      text = lines.join('\n');
    }
    return text.replace(/^[ \t]*PID[ \t]+PGID[ \t]+SID[ \t]+PPID[ \t]+STAT[ \t]+COMMAND$/gm, 'PID PGID SID PPID STAT COMMAND')
      .replace(/^[ \t]*(\d+)[ \t]+(\d+)[ \t]+(\d+)[ \t]+(\d+)[ \t]+(\S+)[ \t]+(.*)$/gm,
        (_, pid, pgid, sid, ppid, state, args) => [pid, pgid, sid, ppid].map(n => this.labels.get(Number(n)) ?? '{{PID}}').join(' ') + ` ${state} ${args}`);
  }
  async afterExit() {
    await Promise.race([this.exited, delay(3000)]);
    await this.snapshot('after_cli_exit');
    clearInterval(this.interval);
    await this.pending;
  }
  async cleanup() {
    clearInterval(this.interval);
    await this.pending;
    const owned = new Map([...this.owned, ...this.parents]);
    for (const [pid, start] of owned) {
      const info = await proc(pid);
      if (!info || info.start !== start || info.stat === 'Z') continue;
      try { process.kill(pid, 'SIGTERM'); this.data.cleanup_signals.push({ pid, signal: 'SIGTERM' }); } catch {}
    }
    await delay(100);
    for (const [pid, start] of owned) {
      const info = await proc(pid);
      if (info?.start === start && info.stat !== 'Z') {
        try { process.kill(pid, 'SIGKILL'); this.data.cleanup_signals.push({ pid, signal: 'SIGKILL' }); } catch {}
      }
    }
  }
}
