import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { startBackend } from "./api/backend"
import { ApiProvider } from "./api/provider"
import { createQueryClient } from "./api/query-client"
import { App } from "./App"
import { ThemeProvider } from "./components/theme/theme-provider"
import { startI18n } from "./i18n/preference"
import { installOverlayScrollbars } from "./lib/overlay-scrollbars"
import { createAppRouter } from "./router"
import { startCodeFont } from "./settings/code-font"
import "./index.css"

// The router is created after the mock has handled its URL switches (see
// api/backend.ts). The language and the code font are settled before anything renders.
startI18n()
startCodeFont()
// Thin overlay thumbs where scrollbars take width, as on Windows.
installOverlayScrollbars()
const backend = startBackend()
const queryClient = createQueryClient()
const router = createAppRouter()
// Another tab changed the mock server's data: refetch, as a pushed update would.
backend.onExternalChange(() => void queryClient.invalidateQueries({ predicate: (q) => q.queryKey[0] !== "auth" }))

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      <ApiProvider client={backend.client} queryClient={queryClient}>
        <App router={router} />
      </ApiProvider>
    </ThemeProvider>
  </StrictMode>,
)
