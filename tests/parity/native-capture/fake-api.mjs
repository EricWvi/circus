import http from 'node:http';

// Adapted from phase0/a: auxiliary calls must never consume the tool script.
export async function startFakeApi({ marker, model, onTurn, onMessages = () => {}, onError = () => {} }) {
  const records = [];
  let sequence = 0;
  let failure;
  let conversationStarted = false;
  const usage = { input_tokens: 25, output_tokens: 10, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 };
  function reply(res, body, turn) {
    const id = ++sequence;
    const content = turn.tool
      ? [{ type: 'tool_use', id: turn.id, name: turn.tool, input: turn.input }]
      : [{ type: 'text', text: turn.text }];
    const message = { id: `msg_fake_${id}`, type: 'message', role: 'assistant', model: body.model || model,
      content, stop_reason: turn.tool ? 'tool_use' : 'end_turn', stop_sequence: null, usage };
    if (!body.stream) return res.end(JSON.stringify(message));
    res.setHeader('content-type', 'text/event-stream');
    res.setHeader('cache-control', 'no-cache');
    const event = data => res.write(`event: ${data.type}\ndata: ${JSON.stringify(data)}\n\n`);
    event({ type: 'message_start', message: { ...message, content: [], stop_reason: null, usage: { ...usage, output_tokens: 1 } } });
    content.forEach((block, index) => {
      event({ type: 'content_block_start', index, content_block: block.type === 'text' ? { type: 'text', text: '' } : { ...block, input: {} } });
      const value = block.type === 'text' ? block.text : JSON.stringify(block.input);
      const split = Math.max(1, Math.floor(value.length / 2));
      for (const chunk of [value.slice(0, split), value.slice(split)]) {
        event({ type: 'content_block_delta', index, delta: block.type === 'text'
          ? { type: 'text_delta', text: chunk } : { type: 'input_json_delta', partial_json: chunk } });
      }
      event({ type: 'content_block_stop', index });
    });
    event({ type: 'message_delta', delta: { stop_reason: message.stop_reason, stop_sequence: null }, usage: { output_tokens: 10 } });
    event({ type: 'message_stop' });
    res.end();
  }
  const server = http.createServer(async (req, res) => {
    try {
      let raw = '';
      // Decode across network chunk boundaries; Buffer.toString per chunk can
      // corrupt a multibyte tool result when a UTF-8 character straddles chunks.
      req.setEncoding('utf8');
      for await (const chunk of req) raw += chunk;
      const body = raw ? JSON.parse(raw) : {};
      const url = new URL(req.url, 'http://localhost');
      const messages = req.method === 'POST' && url.pathname === '/v1/messages';
      const compaction = Array.isArray(body.messages?.at(-1)?.content) && body.messages.at(-1).content.some(b =>
        b.type === 'text' && b.text.startsWith('CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.'));
      const main = Boolean(messages && !compaction && body.model === model && body.tools?.some(t => t.name === 'Read') &&
        (conversationStarted || body.messages?.some(m => JSON.stringify(m.content).includes(marker))));
      if (main) conversationStarted = true;
      // Headers (including credentials) are deliberately never recorded.
      records.push({ method: req.method, path: url.pathname, query: url.search, main, body });
      res.setHeader('content-type', 'application/json');
      if (messages) {
        await onMessages(body, { main, compaction });
        return reply(res, body, main ? await onTurn(body) : { text: 'Auxiliary response' });
      }
      if (req.method === 'POST' && url.pathname === '/v1/messages/count_tokens') return res.end(JSON.stringify({ input_tokens: 25 }));
      throw new Error(`Unexpected fake API endpoint: ${req.method} ${url.pathname}`);
    } catch (error) {
      failure = error;
      onError(error);
      res.statusCode = 400;
      res.end(JSON.stringify({ type: 'error', error: { type: 'invalid_request_error', message: error.message } }));
    }
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  return {
    url: `http://127.0.0.1:${server.address().port}`, records,
    check() { if (failure) throw failure; },
    close: () => new Promise(resolve => { server.close(resolve); server.closeIdleConnections(); }),
  };
}
