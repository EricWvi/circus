export function sizeTextCase(size, multiline = false, name = `probe-${size}`) {
  return {
    id: `read-size-text-${name}${multiline ? '-lines' : ''}`,
    description: `Default Read size text for exactly ${size} bytes, ${multiline ? '100-byte lines including LF and a possible partial last line' : 'one x line without LF'}.`,
    setup: { files: {}, dirs: [], generate: { path: 'size.txt', byte: 'x', size, newline: false, ...(multiline ? { line_bytes: 100 } : {}) } },
    steps: [{ tool: 'Read', input: { file_path: '{{CWD}}/size.txt' } }],
  };
}

// Exact total-file-byte boundary established by measure-size-text.mjs.
export const sizeTextCases = [
  sizeTextCase(10485759, false, 'boundary-real'),
  sizeTextCase(10485760, false, 'boundary-bounded'),
];
