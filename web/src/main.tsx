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
import { ConversationStoreContext } from "./state/conversations-context"
import { DeviceSimulationContext } from "./state/device-simulation-context"
import "./index.css"

// The control plane doesn't serve sessions yet; the mock simulates them (see
// api/backend.ts). The router is created after the mock has handled its
// URL switches.
// The language and the code font are settled before anything renders.
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
        <ConversationStoreContext.Provider value={backend.conversations}>
          <DeviceSimulationContext.Provider value={import.meta.env.DEV ? backend.simulation : null}>
            <App router={router} />
          </DeviceSimulationContext.Provider>
        </ConversationStoreContext.Provider>
      </ApiProvider>
    </ThemeProvider>
  </StrictMode>,
)
