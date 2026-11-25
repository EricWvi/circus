import { create } from "@bufbuild/protobuf";
import { WorkerEventSchema } from "@circus/proto/circus/v1/worker_link_pb";
import { expect, it } from "vitest";
import { HintQueue } from "../src/link/hints.js";

it("bounds the number of distinct hints and still merges duplicates at capacity", () => {
  const queue = new HintQueue();
  const offer = (id: number) => create(WorkerEventSchema, {
    event: { case: "offer", value: { sessionId: String(id) } },
  });
  for (let i = 0; i < 256; i++) queue.push(offer(i));
  expect(() => queue.push(offer(255))).not.toThrow();
  expect(() => queue.push(offer(256))).toThrow("hint queue full");
});

it("keeps LeaseRevoked hints across epochs but merges duplicates within an epoch", async () => {
  const queue = new HintQueue();
  for (const epoch of [3n, 4n, 3n]) {
    queue.push(create(WorkerEventSchema, {
      event: { case: "leaseRevoked", value: { sessionId: "s", epoch } },
    }));
  }
  queue.ready();
  const hints = queue.iterate()[Symbol.asyncIterator]();
  for (const epoch of [3n, 4n]) {
    await expect(hints.next()).resolves.toMatchObject({ value: { event: { value: { epoch } } } });
  }
  const last = hints.next();
  queue.close();
  await expect(last).resolves.toMatchObject({ done: true });
});

it("drops a ReleaseIdle when a Nudge for the session comes before it is consumed, and ignores one behind a Nudge", async () => {
  const queue = new HintQueue();
  const release = (sessionId: string, epoch: bigint) => create(WorkerEventSchema, { event: { case: "releaseIdle", value: { sessionId, epoch } } });
  const nudge = (sessionId: string) => create(WorkerEventSchema, { event: { case: "nudge", value: { sessionId, reasons: [1], interruptTurnNo: 0n } } });
  queue.push(release("a", 3n));
  queue.push(release("b", 4n));
  queue.push(release("b", 5n));
  queue.push(nudge("a"));
  queue.push(nudge("c"));
  queue.push(release("c", 2n));
  queue.ready();
  const hints = queue.iterate()[Symbol.asyncIterator]();
  // b's latest request (merged), then the two Nudges; a's and c's requests are gone.
  await expect(hints.next()).resolves.toMatchObject({ value: { event: { case: "releaseIdle", value: { sessionId: "b", epoch: 5n } } } });
  await expect(hints.next()).resolves.toMatchObject({ value: { event: { case: "nudge", value: { sessionId: "a" } } } });
  await expect(hints.next()).resolves.toMatchObject({ value: { event: { case: "nudge", value: { sessionId: "c" } } } });
  const last = hints.next();
  queue.close();
  await expect(last).resolves.toMatchObject({ done: true });
});
