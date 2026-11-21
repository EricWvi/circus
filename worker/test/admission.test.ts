import { expect, it, vi } from "vitest";
import { SendAdmission } from "../src/link/admission.js";

it("drains the queue when an aborted head was blocking a request that fits", async () => {
  const admission = new SendAdmission(100, 4);
  await admission.take(70, 0);
  const abort = new AbortController();
  const reason = new Error("abandon head");
  const head = expect(admission.take(50, 0, abort.signal)).rejects.toBe(reason);
  const grant = vi.fn();
  void admission.take(20, 0).then(grant);
  await Promise.resolve();
  expect(grant).not.toHaveBeenCalled();

  abort.abort(reason);
  await head;
  expect(grant).toHaveBeenCalledOnce();
});

it("grants every waiter that fits after a single release", async () => {
  const admission = new SendAdmission(100, 4);
  const full = await admission.take(100, 0);
  const grant = vi.fn();
  for (const request of [10, 20, 30]) {
    void admission.take(request, 0).then(() => grant(request));
  }
  await Promise.resolve();
  expect(grant).not.toHaveBeenCalled();

  full.release();
  await Promise.resolve();
  expect(grant.mock.calls).toEqual([[10], [20], [30]]);
});

it("rejects queued waiters on close and never grants them later", async () => {
  const admission = new SendAdmission(100, 4);
  const full = await admission.take(100, 0);
  const error = new Error("link closed");
  const canceled = vi.fn();
  const granted = vi.fn();
  for (const request of [50, 20]) {
    void admission.take(request, 0).then(granted, canceled);
  }
  admission.close(error);
  full.release();
  await Promise.resolve();
  expect(canceled.mock.calls).toEqual([[error], [error]]);
  expect(granted).not.toHaveBeenCalled();
});

it("keeps the grant cap after the request bytes are written", async () => {
  const admission = new SendAdmission(100, 2);
  const first = await admission.take(10, 5);
  const second = await admission.take(10, 5);
  first.written();
  second.written();
  const grant = vi.fn();
  void admission.take(10, 5).then(grant);
  await Promise.resolve();
  expect(grant).not.toHaveBeenCalled();

  first.release();
  await Promise.resolve();
  expect(grant).toHaveBeenCalledOnce();
});
