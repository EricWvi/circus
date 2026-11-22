import type { SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";

/**
 * PushQueue is the streaming prompt of one query: messages are handed to the
 * SDK in push order. The SDK pulls as fast as it can, so the queue holds at
 * most the messages pushed since its last read.
 */
export class PushQueue implements AsyncIterable<SDKUserMessage> {
  #items: SDKUserMessage[] = [];
  #wake?: () => void;
  #ended = false;
  #iterating = false;

  get size(): number {
    return this.#items.length;
  }

  push(message: SDKUserMessage): void {
    if (this.#ended) throw new Error("push queue has ended");
    this.#items.push(message);
    this.#wake?.();
  }

  end(): void {
    this.#ended = true;
    this.#wake?.();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<SDKUserMessage> {
    if (this.#iterating) throw new Error("push queue already has a reader");
    this.#iterating = true;
    for (;;) {
      while (this.#items.length) yield this.#items.shift()!;
      if (this.#ended) return;
      const { promise, resolve } = Promise.withResolvers<void>();
      this.#wake = resolve;
      await promise;
      this.#wake = undefined;
    }
  }
}
