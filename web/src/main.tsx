import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./App"
import { ThemeProvider } from "./components/theme/theme-provider"
import { SessionsProvider } from "./state/sessions-provider"
import "./index.css"

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <ThemeProvider>
      <SessionsProvider>
        <App />
      </SessionsProvider>
    </ThemeProvider>
  </StrictMode>,
)
