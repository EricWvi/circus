import { createReadStream } from 'node:fs';
import { mkdir, readdir, writeFile } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { homedir } from 'node:os';
import { basename, join, resolve, normalize, isAbsolute } from 'node:path';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';

export const repo = resolve(import.meta.dirname, '../../..');
const allowed = new Set(['Bash', 'Read', 'Write', 'Edit']);
export const canonical = value => JSON.stringify(value, (_, item) => item && typeof item === 'object' && !Array.isArray(item)
  ? Object.fromEntries(Object.entries(item).sort(([a], [b]) => a.localeCompare(b))) : item);

export async function extractSessions({ source = join(homedir(), '.claude/projects'), output = join(repo, '.tmp/diff-corpus/sessions.jsonl') } = {}) {
  const unique = new Map();
  let malformed = 0;
  async function walk(dir) {
    for (const entry of (await readdir(dir, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name))) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) await walk(path);
      else if (entry.isFile() && entry.name.endsWith('.jsonl')) {
        for await (const line of createInterface({ input: createReadStream(path), crlfDelay: Infinity })) {
          let row;
          try { row = JSON.parse(line); } catch { malformed++; continue; }
          // Only assistant message tool-use inputs are retained, never results or text.
          for (const block of row.message?.role === 'assistant' && Array.isArray(row.message.content) ? row.message.content : []) {
            if (block.type !== 'tool_use' || !allowed.has(block.name) || !block.input || typeof block.input !== 'object') continue;
            if (Buffer.byteLength(JSON.stringify(block.input)) > 64 * 1024) continue;
            const value = { tool: block.name, input: block.input };
            unique.set(canonical(value), value);
          }
        }
      }
    }
  }
  await walk(source);
  await mkdir(resolve(output, '..'), { recursive: true, mode: 0o700 });
  await writeFile(output, [...unique.values()].map(value => JSON.stringify(value)).join('\n') + (unique.size ? '\n' : ''), { mode: 0o600 });
  return { inputs: unique.size, malformed };
}

export function sessionCase({ tool, input }, index) {
  if (!allowed.has(tool) || !input || typeof input !== 'object' || Buffer.byteLength(JSON.stringify(input)) > 65536) throw new Error('Invalid corpus input');
  const hash = createHash('sha256').update(canonical({ tool, input })).digest('hex').slice(0, 12);
  input = { ...input };
  if (tool !== 'Bash') {
    if (typeof input.file_path !== 'string') throw new Error('Missing file_path');
    const path = input.file_path.startsWith('{{CWD}}/') ? input.file_path.slice(8) : input.file_path;
    const cleaned = normalize(path);
    if (isAbsolute(path) || cleaned === '..' || cleaned.startsWith('../')) {
      const name = basename(path);
      input.file_path = `{{CWD}}/${!name || ['.', '..'].includes(name) ? 'file.txt' : name}`;
    }
  }
  const steps = [{ tool, input }];
  if (tool === 'Edit') {
    if (typeof input.old_string !== 'string') throw new Error('Missing old_string');
    steps.unshift({ tool: 'Write', input: { file_path: input.file_path, content: input.old_string } });
  }
  return { id: `session-${index + 1}-${hash}`, description: 'Replay an extracted tool input in a fresh project.', setup: { files: {}, dirs: [] }, steps };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  if (process.argv.length !== 2) throw new Error('Usage: node extract-sessions.mjs');
  const result = await extractSessions();
  console.log(`Extracted ${result.inputs} unique tool inputs; ignored ${result.malformed} malformed lines.`);
}
