export const readStep = (extra = {}) => ({ tool: 'Read', input: { file_path: '{{CWD}}/threshold.txt', ...extra } });
export const bashStep = command => ({ tool: 'Bash', input: { command, description: 'Measure native output threshold' } });

export function thresholdCase(family, size, { id = `${family}-${size}`, readInput = {} } = {}) {
  let content;
  let command;
  if (family === 'read-ascii-lines') content = Array.from({ length: size }, (_, i) => `line ${i + 1}\n`).join('');
  else if (family === 'read-long-line') content = 'x'.repeat(size);
  else if (family === 'read-cjk-lines') content = '中文测试行\n'.repeat(size);
  else if (family === 'bash-ascii') command = `head -c ${size} /dev/zero | tr '\\0' a`;
  else if (family === 'bash-cjk') command = `printf '%s' "$(head -c ${size} /dev/zero | tr '\\0' a)" | sed 's/a/中/g'`;
  else if (family === 'bash-many-lines') command = `seq 1 ${size}`;
  else if (family === 'bash-split') command = `head -c ${Math.floor(size / 2)} /dev/zero | tr '\\0' a; head -c ${Math.ceil(size / 2)} /dev/zero | tr '\\0' b >&2`;
  else throw new Error(`Unknown threshold family: ${family}`);
  return {
    id, description: `Measure ${family} with size ${size}.`,
    setup: { files: content === undefined ? {} : { 'threshold.txt': content }, dirs: [] },
    steps: [content === undefined ? bashStep(command) : readStep(readInput)],
  };
}

export function classify(fixture, family, size) {
  const result = fixture.steps[0].result;
  const text = result.content;
  const body = text.split('\n\n<system-reminder>\n<total_tokens>')[0];
  let outcome = result.is_error ? 'error' : body.startsWith('<persisted-output>') ? 'persisted' : 'inline';
  if (outcome === 'inline' && family.startsWith('read-')) {
    const content = fixture.setup.files['threshold.txt'];
    const expected = content.split('\n').map((line, i) => `${i + 1}${line ? `\t${line}` : ''}`).join('\n');
    if (body !== expected) outcome = 'partial';
  }
  if (outcome === 'inline' && family.startsWith('bash-')) {
    const expected = family === 'bash-ascii' ? 'a'.repeat(size) : family === 'bash-cjk' ? '中'.repeat(size)
      : family === 'bash-split' ? 'a'.repeat(Math.floor(size / 2)) + 'b'.repeat(Math.ceil(size / 2))
        : Array.from({ length: size }, (_, i) => `${i + 1}`).join('\n');
    if (body !== expected) outcome = 'partial';
  }
  return {
    size, outcome, is_error: result.is_error,
    ...(family.startsWith('read-') ? {
      bytes: Buffer.byteLength(fixture.setup.files['threshold.txt']),
      characters: fixture.setup.files['threshold.txt'].length,
    } : { bytes: family === 'bash-cjk' ? size * 3 : family === 'bash-many-lines'
      ? Buffer.byteLength(Array.from({ length: size }, (_, i) => `${i + 1}\n`).join('')) : size,
      characters: family === 'bash-many-lines' ? Array.from({ length: size }, (_, i) => `${i + 1}\n`).join('').length : size }),
    ...(outcome === 'error' || outcome === 'partial' ? { text: body.slice(0, 1200) } : {}),
  };
}

const boundaries = {
  'read-ascii-lines': { stem: 'read-threshold-ascii', sizes: [24840, 24841] },
  'read-long-line': { stem: 'read-threshold-long-line', sizes: [262144, 262145] },
  'read-cjk-lines': { stem: 'read-threshold-cjk', sizes: [16384, 16385] },
  'bash-ascii': { stem: 'bash-threshold-ascii', sizes: [30000, 30001] },
  'bash-cjk': { stem: 'bash-threshold-cjk', sizes: [10000, 10001] },
};
export const thresholdCases = Object.entries(boundaries).flatMap(([family, { stem, sizes }]) => sizes.map((size, i) =>
  thresholdCase(family, size, { id: `${stem}-${i ? 'over' : 'inline'}` })));
thresholdCases.push(
  thresholdCase('read-ascii-lines', 24841, { id: 'read-threshold-range', readInput: { offset: 10, limit: 5 } }),
  ...[
    ['read-offset-zero', { offset: 0 }],
    ['read-limit-zero', { limit: 0 }],
    ['read-offset-one-limit-one', { offset: 1, limit: 1 }],
  ].map(([id, extra]) => ({
    id, description: `Measure Read edge input ${JSON.stringify(extra)}.`,
    setup: { files: { 'threshold.txt': 'alpha\nbeta\ngamma\ndelta\nepsilon\n' }, dirs: [] }, steps: [readStep(extra)],
  })),
  thresholdCase('bash-split', 30001, { id: 'bash-threshold-split' }),
  thresholdCase('bash-many-lines', 6221, { id: 'bash-threshold-many-lines' }),
);
