import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { loadEnv, type Plugin, type ProxyOptions } from "vite"
import { defineConfig } from "vitest/config"

const DEFAULT_SERVER = "http://127.0.0.1:8443"

/**
 * In server mode the dev server forwards the control plane's routes, so the
 * browser sees one origin (the Vite one, which the server's dev config uses
 * as `public_url`). Host and Origin pass through unchanged for the server's
 * Origin check; cookies carry no Domain and redirects use relative paths,
 * so both stay on the Vite origin without rewriting.
 */
function controlPlaneProxy(target: string): Record<string, ProxyOptions> {
  const options: ProxyOptions = { target, changeOrigin: false }
  return {
    // Connect RPCs: /circus.v1.<Service>/<Method>
    "/circus.v1.": options,
    // The OpenID Connect redirects.
    "/auth/": options,
  }
}

const serverBackendMarker: Plugin = {
  name: "circus-server-backend",
  transformIndexHtml: () => [
    {
      tag: "meta",
      attrs: { name: "circus-backend", content: "server" },
      injectTo: "head",
    },
  ],
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, import.meta.dirname, "VITE_")
  const serverMode = env.VITE_CIRCUS_BACKEND === "server"

  return {
    plugins: [react(), tailwindcss(), serverMode && serverBackendMarker],
    resolve: {
      alias: [
        { find: "@", replacement: path.resolve(import.meta.dirname, "./src") },
        // The generated protocol code is compiled from source together with
        // the app, so `gen/ts` needs no build step of its own.
        {
          find: /^@circus\/proto\/(.*)$/,
          replacement: path.resolve(import.meta.dirname, "../gen/ts/src/$1.ts"),
        },
      ],
    },
    server: serverMode
      ? {
          proxy: controlPlaneProxy(env.VITE_CIRCUS_SERVER || DEFAULT_SERVER),
          // The origin must stay the server's public_url; fail instead of
          // moving to another port.
          strictPort: true,
        }
      : undefined,
    test: {
      environment: "node",
      include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
      setupFiles: ["src/test/setup.ts"],
    },
  }
})
