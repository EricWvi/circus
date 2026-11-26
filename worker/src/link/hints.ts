import { create } from "@bufbuild/protobuf";
import {
  WorkerEvent_Nudge_Reason as Reason, WorkerEventSchema, type WorkerEvent,
} from "@circus/proto/circus/v1/worker_link_pb";
import { ProtocolError } from "../wire/codec.js";

export const MAX_HINTS = 256;
export const validSessionId = (id: string): boolean => /^[A-Za-z0-9_-]{1,128}$/.test(id);

export class HintQueue {
  #events = new Map<string, WorkerEvent>();
  #waiting?: ReturnType<typeof Promise.withResolvers<IteratorResult<WorkerEvent>>>;
  #ready = false;
  #closed = false;
  #iterating = false;

  push(message: WorkerEvent): void {
    if (this.#closed) return;
    const { event } = message;
    if (event.case === "agentsWanted") {
      // The latest wishes replace any not yet consumed.
      if (!this.#events.has(event.case) && this.#events.size >= MAX_HINTS) throw new Error("worker hint queue full");
      this.#events.set(event.case, message);
      this.#deliver();
      return;
    }
    if (event.case === undefined || !validSessionId(event.value.sessionId)) {
      throw new ProtocolError("invalid worker event");
    }
    const id = event.value.sessionId;
    // A Nudge means the session has work: a ReleaseIdle not yet consumed
    // is dropped, and one arriving while a Nudge waits is ignored.
    if (event.case === "nudge") this.#events.delete(`releaseIdle/${id}`);
    if (event.case === "releaseIdle" && this.#events.has(`nudge/${id}`)) return;
    const key = event.case === "leaseRevoked"
      ? `${event.case}/${id}/${event.value.epoch}`
      : `${event.case}/${id}`;
    const previous = this.#events.get(key)?.event;
    if (!previous && this.#events.size >= MAX_HINTS) throw new Error("worker hint queue full");
    if (event.case === "nudge") {
      const earlier = previous?.case === "nudge" ? previous.value : undefined;
      const reasons = [...new Set([...(earlier?.reasons ?? []), ...event.value.reasons])]
        .filter((reason) => reason >= Reason.INPUT && reason <= Reason.APPROVAL);
      const before = earlier?.interruptTurnNo ?? 0n;
      const after = event.value.interruptTurnNo;
      message = create(WorkerEventSchema, {
        event: { case: "nudge", value: { sessionId: id, reasons, interruptTurnNo: before > after ? before : after } },
      });
    }
    this.#events.set(key, message);
    this.#deliver();
  }

  ready(): void {
    this.#ready = true;
    this.#deliver();
  }

  reset(): void {
    this.#ready = false;
    this.#events.clear();
  }

  close(): void {
    this.#closed = true;
    this.reset();
    this.#deliver();
  }

  iterate(): AsyncIterable<WorkerEvent> {
    return {
      [Symbol.asyncIterator]: () => {
        if (this.#iterating) throw new Error("worker hints already have a consumer");
        this.#iterating = true;
        let ended = false;
        return {
          next: () => {
            if (ended || this.#closed) return Promise.resolve({ done: true, value: undefined });
            if (this.#waiting) return Promise.reject(new Error("worker hints already have a pending read"));
            const waiting = Promise.withResolvers<IteratorResult<WorkerEvent>>();
            this.#waiting = waiting;
            this.#deliver();
            return waiting.promise;
          },
          return: () => {
            if (!ended) {
              ended = true;
              this.#iterating = false;
              this.#waiting?.resolve({ done: true, value: undefined });
              this.#waiting = undefined;
            }
            return Promise.resolve({ done: true, value: undefined });
          },
        };
      },
    };
  }

  #deliver(): void {
    if (!this.#waiting) return;
    if (this.#closed) {
      this.#waiting.resolve({ done: true, value: undefined });
    } else {
      if (!this.#ready) return;
      const first = this.#events.entries().next();
      if (first.done) return;
      const [key, value] = first.value;
      this.#events.delete(key);
      this.#waiting.resolve({ done: false, value });
    }
    this.#waiting = undefined;
  }
}
