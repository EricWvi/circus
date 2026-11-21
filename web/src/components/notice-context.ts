import { createContext, useContext } from "react"

/** One line of a notice, with the device's own words when there are any. */
export interface NoticeLine {
  text: string
  /** Shown apart, in the code font. */
  detail?: string
}

/**
 * Something that happened that the page the user is on now didn't show.
 * Its text is made when it renders, so it follows the language.
 */
export interface Notice {
  content: () => { title: string; lines: NoticeLine[] }
  /**
   * It reports a failure: it stays until the user has done something after
   * it appeared (and so has had it in view), then goes after a while like
   * the others. Either kind can be closed at once.
   */
  failure: boolean
}

export interface ShownNotice extends Notice {
  id: number
}

export interface Notices {
  show(notice: Notice): void
  list: readonly ShownNotice[]
  dismiss(id: number): void
}

export const NoticeContext = createContext<Notices | null>(null)

function useNotices(): Notices {
  const notices = useContext(NoticeContext)
  if (!notices) throw new Error("notices must be used within NoticeProvider")
  return notices
}

/** Shows a notice at the top of the page area. */
export const useShowNotice = (): Notices["show"] => useNotices().show

/** The notices being shown, and how to close one. */
export const useNoticeList = () => {
  const { list, dismiss } = useNotices()
  return { list, dismiss }
}
