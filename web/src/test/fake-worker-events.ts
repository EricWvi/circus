// The recorded session events of the Go fake worker's scripted scenarios,
// as the client receives them. The fixture is written and checked for drift
// by TestWebEventFixture (internal/testutil/fakeworker/webfixture_test.go):
// what WatchSession streamed from a real control plane, with ids numbered,
// and timestamps, durations and usage removed.

import type { RawSessionEvent, SessionEventKind } from "@/api/types"
import fixture from "@/lib/session-events/fake-worker.fixture.json"

export type Scenario = keyof typeof fixture

/** A scenario's events, 400 ms apart from `startMs`. */
export function fakeWorkerEvents(scenario: Scenario, startMs = Date.parse("2026-11-21T10:00:00Z")): RawSessionEvent[] {
  return fixture[scenario].events.map((e, i) => ({
    seq: e.seq,
    tsUnixMs: startMs + i * 400,
    kind: e.kind.replace(/^KIND_/, "").toLowerCase() as SessionEventKind,
    payloadJson: e.payload === null ? "" : JSON.stringify(e.payload),
    ephemeral: e.ephemeral,
    truncated: e.truncated,
  }))
}

/** How the session ended up after the scenario. */
export const fakeWorkerFinal = (scenario: Scenario) => fixture[scenario].final
