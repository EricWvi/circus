import { createContext, useContext } from "react"

/** Opens the new-project dialog. */
export const NewProjectContext = createContext<(() => void) | null>(null)

export function useOpenNewProject(): () => void {
  const open = useContext(NewProjectContext)
  if (!open) throw new Error("useOpenNewProject must be used within NewProjectDialogProvider")
  return open
}
