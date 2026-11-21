export interface SendPermit {
  written(): void;
  release(): void;
}

interface Waiter {
  bytes: number;
  tail: number;
  resolve: (permit: SendPermit) => void;
  reject: (error: unknown) => void;
  cleanup: () => void;
}

// The operation table bounds waiters. Grants also account for operations
// whose slots were returned after cancellation but whose Unwatch is queued.
export class SendAdmission {
  #bytes = 0;
  #grants = 0;
  #waiters: Waiter[] = [];

  constructor(readonly capacity: number, readonly maxGrants: number) {}

  take(size: number, tail: number, signal?: AbortSignal): Promise<SendPermit> {
    const bytes = size + tail;
    if (bytes > this.capacity) throw new RangeError(`worker request exceeds the send byte budget (${this.capacity} bytes)`);
    // Dispatch all grants via the same microtask path. A new caller must
    // not send ahead of an older waiter whose grant was just resolved.
    if (this.#waiters.length === 0 && this.#fits(bytes)) return Promise.resolve(this.#grant(bytes, tail));

    const { promise, resolve, reject } = Promise.withResolvers<SendPermit>();
    const abort = () => {
      const position = this.#waiters.indexOf(waiter);
      if (position === -1) return;
      this.#waiters.splice(position, 1);
      waiter.cleanup();
      reject(signal!.reason);
      this.#drain();
    };
    const waiter = { bytes, tail, resolve, reject, cleanup: () => signal?.removeEventListener("abort", abort) };
    this.#waiters.push(waiter);
    signal?.addEventListener("abort", abort, { once: true });
    return promise;
  }

  close(error: Error): void {
    for (const waiter of this.#waiters.splice(0)) {
      waiter.cleanup();
      waiter.reject(error);
    }
  }

  #fits(bytes: number): boolean {
    return bytes <= this.capacity - this.#bytes && this.#grants < this.maxGrants;
  }

  #grant(bytes: number, tail: number): SendPermit {
    this.#bytes += bytes;
    this.#grants++;
    let charged = bytes;
    return {
      written: () => {
        if (charged <= tail) return;
        this.#bytes -= charged - tail;
        charged = tail;
        this.#drain();
      },
      release: () => {
        if (charged === 0) return;
        this.#bytes -= charged;
        charged = 0;
        this.#grants--;
        this.#drain();
      },
    };
  }

  #drain(): void {
    while (this.#waiters.length && this.#fits(this.#waiters[0].bytes)) {
      const waiter = this.#waiters.shift()!;
      waiter.cleanup();
      waiter.resolve(this.#grant(waiter.bytes, waiter.tail));
    }
  }
}
