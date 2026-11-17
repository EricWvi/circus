import type { en } from "./locales/en"

type Widen<T> = { readonly [K in keyof T]: T[K] extends string ? string : Widen<T[K]> }

/**
 * The shape every catalog has: English's keys, any text as values. A
 * catalog declared with `satisfies Catalog` fails to typecheck when it
 * misses a key or has one English doesn't.
 */
export type Catalog = Widen<typeof en>
