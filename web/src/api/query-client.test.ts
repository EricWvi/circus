import { describe, expect, it, onTestFinished } from "vitest"

import { ApiError } from "./errors"
import { reportIfUnauthenticated } from "./unauthenticated"
import {
  announceAuthChange,
  createQueryClient,
  followAuthChanges,
  followUnauthenticated,
  queryKeys,
  resetAuth,
  type CachedAuth,
} from "./query-client"

const signedIn = (userId: string): CachedAuth => ({
  kind: "signed_in",
  user: { userId, email: `${userId}@example.com`, role: "user" },
})

describe("createQueryClient", () => {
  it("signs out when a request is rejected as unauthenticated", async () => {
    const qc = createQueryClient()
    onTestFinished(followUnauthenticated(qc))
    resetAuth(qc, signedIn("a"))
    await qc
      .fetchQuery({
        queryKey: queryKeys.projects,
        queryFn: () => Promise.reject(new ApiError("unauthenticated", "expired")),
      })
      .catch(() => {})
    expect(qc.getQueryData(queryKeys.auth)).toEqual({ kind: "signed_out" })
  })

  it("signs out when any call outside React Query is rejected as unauthenticated, while followed", () => {
    const qc = createQueryClient()
    const stop = followUnauthenticated(qc)
    resetAuth(qc, signedIn("a"))
    qc.setQueryData(queryKeys.session("s1"), { title: "x" })
    // What the Connect client does for every call and stream (connect/call.ts).
    reportIfUnauthenticated(new ApiError("unavailable", "down"))
    expect(qc.getQueryData(queryKeys.auth)).toEqual(signedIn("a"))
    reportIfUnauthenticated(new ApiError("unauthenticated", "signed out elsewhere"))
    expect(qc.getQueryData(queryKeys.auth)).toEqual({ kind: "signed_out" })
    expect(qc.getQueryData(queryKeys.session("s1"))).toBeUndefined()

    stop()
    resetAuth(qc, signedIn("a"))
    reportIfUnauthenticated(new ApiError("unauthenticated", "after unsubscribing"))
    expect(qc.getQueryData(queryKeys.auth)).toEqual(signedIn("a"))
  })

  it("drops other data without refetching when a refetch finds the user signed out", () => {
    const qc = createQueryClient()
    resetAuth(qc, signedIn("a"))
    qc.setQueryData(queryKeys.projects, ["a's project"])
    qc.setQueryData<CachedAuth>(queryKeys.auth, { kind: "signed_out" })
    expect(qc.getQueryCache().find({ queryKey: queryKeys.projects })).toBeUndefined()
  })

  it("drops other data when a refetch finds a different user", async () => {
    const qc = createQueryClient()
    resetAuth(qc, signedIn("a"))
    qc.setQueryData(queryKeys.projects, ["a's project"])
    qc.setQueryData(queryKeys.auth, signedIn("b"))
    await Promise.resolve()
    expect(qc.getQueryData(queryKeys.projects)).toBeUndefined()
  })

  it("keeps data when a refetch finds the same user", () => {
    const qc = createQueryClient()
    resetAuth(qc, signedIn("a"))
    qc.setQueryData(queryKeys.projects, ["a's project"])
    qc.setQueryData(queryKeys.auth, signedIn("a"))
    expect(qc.getQueryData(queryKeys.projects)).toEqual(["a's project"])
  })
})

describe("resetAuth", () => {
  it("wins over a whoAmI that was already in flight", async () => {
    const qc = createQueryClient()
    qc.setQueryData<CachedAuth>(queryKeys.auth, { kind: "signed_out" })
    let resolve!: (auth: CachedAuth) => void
    const pending = qc
      .fetchQuery({
        queryKey: queryKeys.auth,
        queryFn: () => new Promise<CachedAuth>((r) => (resolve = r)),
        staleTime: 0,
      })
      .catch(() => {})
    resetAuth(qc, signedIn("b"))
    resolve({ kind: "signed_out" })
    await pending
    await new Promise((r) => setTimeout(r, 0))
    const query = qc.getQueryCache().find({ queryKey: queryKeys.auth })
    expect(query?.state.data).toEqual(signedIn("b"))
    expect(query?.state.status).toBe("success")
  })
})

describe("auth broadcasts", () => {
  it("ignore this tab's own announcements and follow other tabs'", async () => {
    const qc = createQueryClient()
    resetAuth(qc, signedIn("a"))
    const stop = followAuthChanges(qc)
    announceAuthChange()
    await new Promise((r) => setTimeout(r, 20))
    expect(qc.getQueryState(queryKeys.auth)?.isInvalidated).toBe(false)

    const other = new BroadcastChannel("circus-auth")
    other.postMessage({ from: "another-tab" })
    other.close()
    await new Promise((r) => setTimeout(r, 20))
    expect(qc.getQueryState(queryKeys.auth)?.isInvalidated).toBe(true)
    stop()
  })
})
