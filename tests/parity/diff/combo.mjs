const quote = value => `'${value.replaceAll("'", "'\\''")}'`;
const octal = bytes => [...bytes].map(byte => `\\${byte.toString(8).padStart(3, '0')}`).join('');
const fileValue = bytes => {
  const text = bytes.toString('utf8');
  return !bytes.includes(0) && Buffer.from(text).equals(bytes) ? text : { base64: bytes.toString('base64') };
};
const bash = command => ({ tool: 'Bash', input: { command, description: 'Exercise composed shell behavior', timeout: 2000 } });

// Payloads are bytes until they cross a string-only tool input boundary. This
// preserves malformed UTF-8 and NUL in shell output and setup files; Write/Edit
// strings use Node's UTF-8 replacement decoding for malformed byte sequences.
export function generateCombo({ seed, count }, rng) {
  const int = max => Math.floor(rng() * max);
  const pick = values => values[int(values.length)];
  const chance = probability => rng() < probability;
  const pool = ['a', 'Z', '0', '9', ' ', '%', "'", '"', '\\', '$', '中', '文', '😀', '\r', '\n', '\r\n', '\t', '\0', '\x1b[31m', '\x1b[0m', Buffer.from([0xff]), Buffer.from([0xc0, 0xaf])].map(value => Buffer.isBuffer(value) ? value : Buffer.from(value));
  const linePool = pool.filter(part => !part.includes(10) && !part.includes(13));
  const textPool = linePool.filter(part => !part.includes(0) && Buffer.from(part.toString('utf8')).equals(part));
  const bytes = (length = int(25), choices = pool) => Buffer.concat(Array.from({ length }, () => pick(choices)));
  const text = (length = int(25)) => bytes(length, chance(0.5) ? textPool : pool).toString('utf8');
  const repeated = (motif, size) => Buffer.concat([Buffer.alloc(Math.floor(size / motif.length) * motif.length).fill(motif), motif.subarray(0, size % motif.length)]);
  const path = name => `{{CWD}}/${name}`;
  const seedID = /^[a-zA-Z0-9_-]+$/.test(String(seed)) ? seed : Buffer.from(String(seed)).toString('hex');

  function fileBytes() {
    if (chance(0.12)) {
      const motif = chance(0.75) ? Buffer.from(pick(['x', '中', '😀', 'xy'])) : bytes(1 + int(6), linePool);
      return repeated(motif, pick([262143, 262144, 262145]));
    }
    const lines = int(61);
    const choices = chance(0.55) ? textPool : chance(0.5) ? linePool : linePool.filter(part => !part.includes(0));
    const parts = [];
    for (let line = 0; line < lines; line++) {
      parts.push(bytes(int(30), choices));
      if (line < lines - 1 || chance(0.5)) parts.push(Buffer.from(pick(['\n', '\r\n', '\r'])));
    }
    return Buffer.concat(parts);
  }

  function printfAtom() {
    if (chance(0.35)) {
      const size = pick([0, 1, 1999, 2000, 2001, 29999, 30000, 30001]);
      const motif = bytes(1 + int(6));
      const repeats = Math.floor(size / motif.length);
      const tail = motif.subarray(0, size % motif.length);
      // Compact source avoids ARG_MAX while retaining exact byte-sized output.
      const commands = [];
      if (repeats) commands.push(`printf ${quote(octal(motif) + '%.0s')} {1..${repeats}}`);
      commands.push(`printf '%b' ${quote(octal(tail))}`);
      return `{ ${commands.join('; ')}; }`;
    }
    return `printf '%b' ${quote(octal(bytes()))}`;
  }

  function command(state) {
    const written = [];
    function atom() {
      const atoms = [
        printfAtom,
        () => `exit ${int(256)}`,
        () => `kill -${pick(['HUP', 'INT', 'TERM', 'PIPE'])} $$`,
        () => `cd ${quote(pick([path('sub'), path('sub/deeper'), path('missing'), '.', '..']))}`,
        () => `export COMBO_VAR=$'${octal(bytes(1 + int(8)))}'`,
        () => 'unset COMBO_VAR',
        () => `set ${pick(['-e', '-u', '-o pipefail'])}`,
        () => pick(['true', 'false']),
        () => `cat ${quote(pick([path('input.bin'), ...state.written]))}`,
        () => `wc ${pick(['-c', '-l', '-w'])} < ${quote(path('input.bin'))}`,
        () => `sort ${quote(path('input.bin'))}`,
        () => `head -c ${pick([0, 1, 17, 1999, 2000, 2001, 29999, 30000, 30001])} /dev/zero`,
        () => 'pwd',
        () => 'printf "[%s]" "${COMBO_VAR-unset}"',
        () => 'printf "%s" "$COMBO_UNSET"',
      ];
      // State observations become more frequent after a preceding tool call.
      const source = state.later && chance(0.35) ? pick([atoms[8], atoms[12], atoms[13]])() : pick(atoms)();
      const wrapped = pick([source, `( ${source} )`, `{ ${source}; }`, `printf '%s' "$( ${source} )"`]);
      if (!chance(0.5)) return wrapped;
      const target = path(`out-${state.step}-${written.length}.bin`);
      const redirects = [`> ${quote(target)}`, `>> ${quote(target)}`, '2>&1', '2>/dev/null', '>&2', `<<< $'${octal(bytes(1 + int(8)))}'`];
      const redirect = pick(redirects);
      if (redirect.startsWith('>')) written.push(target);
      return `${wrapped} ${redirect}`;
    }
    let result = atom();
    const atoms = 1 + int(4);
    for (let i = 1; i < atoms; i++) result += ` ${pick([';', '&&', '||', '|'])} ${atom()}`;
    state.written.push(...written);
    return result;
  }

  function readStep(name, content) {
    const lines = content.split(/\r\n|\n|\r/).length;
    const input = { file_path: chance(0.3) ? name : path(name) };
    if (chance(0.7)) input.offset = pick([0, 1, lines, lines + 1, lines + 1 + int(30), 4294967295]);
    if (chance(0.7)) input.limit = pick([0, 1, lines, lines + 1, 1 + int(60), 4294967295]);
    return { tool: 'Read', input };
  }

  function editStep(name, content) {
    const points = Array.from(content);
    const start = points.length ? int(points.length) : 0;
    let old = points.slice(start, start + 1 + int(16)).join('');
    switch (int(5)) {
      case 0: {
        // Widen a sampled span until it is unique, without inventing an oracle.
        if (old && content.split(old).length !== 2) old = content;
        break;
      }
      case 1: old = pick([...new Set(points)].slice(0, 100).filter(point => content.split(point).length > 2).concat(['a'])); break;
      case 2: {
        const index = points.findIndex(point => point === '\r' || point === '\n');
        if (index >= 0) old = points.slice(Math.max(0, index - 2), index + 3).join('');
        break;
      }
      case 3: old = `absent-${int(1000000)}`; break;
      case 4: old = ''; break;
    }
    const replacement = chance(0.2) ? old : chance(0.25) ? '' : text();
    return { tool: 'Edit', input: { file_path: chance(0.3) ? name : path(name), old_string: old, new_string: replacement, replace_all: chance(0.5) } };
  }

  return Array.from({ length: count }, (_, index) => {
    const definition = { id: `combo-${seedID}-${index + 1}`, description: `Compose independent tool and payload choices with seed ${seed}.`, setup: { files: {}, dirs: ['sub', 'sub/deeper'] }, steps: [] };
    const steps = 1 + int(4);
    const family = pick(['bash', 'bash', 'bash', 'read', 'edit', 'write']);
    if (family === 'bash') {
      definition.setup.files['input.bin'] = fileValue(fileBytes());
      const state = { later: false, written: [], step: 0 };
      for (let i = 0; i < steps; i++) {
        state.later = i > 0;
        state.step = i;
        definition.steps.push(bash(command(state)));
      }
    } else {
      let name = family === 'write' ? pick(['new.txt', 'a.txt', 'nested/missing/new.txt']) : 'a.txt';
      const raw = fileBytes();
      let content = raw.toString('utf8');
      if (family !== 'write' || chance(0.5)) definition.setup.files[name] = fileValue(raw);
      for (let i = 0; i < steps; i++) {
        const kind = i === 0 ? family : pick(['read', 'edit', 'write']);
        if (kind === 'read') definition.steps.push(readStep(name, content));
        else if (kind === 'edit') {
          if (i === 0 && steps > 1 && chance(0.5)) definition.steps.push(readStep(name, content));
          if (definition.steps.length < steps) definition.steps.push(editStep(name, content));
        } else {
          content = text(int(100));
          definition.steps.push({ tool: 'Write', input: { file_path: chance(0.3) ? name : path(name), content } });
        }
        if (definition.steps.length >= steps) break;
      }
    }
    return definition;
  });
}
