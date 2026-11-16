interface ImportMetaEnv {
  /** "mock" (default) or "server"; see `api/backend.ts`. */
  readonly VITE_CIRCUS_BACKEND?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
