// Tests read UI text in English.
import { initI18n } from "@/i18n/i18n"

initI18n("en")

// happy-dom's fetch Response lacks bytes(), which Shiki uses to load its
// inlined WebAssembly when a test renders highlighted code.
if (typeof Response !== "undefined" && !("bytes" in Response.prototype)) {
  Object.defineProperty(Response.prototype, "bytes", {
    async value(this: Response) {
      return new Uint8Array(await this.arrayBuffer())
    },
  })
}
