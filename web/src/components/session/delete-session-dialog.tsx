import { useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { matchPath, useLocation, useNavigate } from "react-router"

import { errorMessage, isApiError, isPreconditionFailure } from "@/api/errors"
import { useDeleteSession } from "@/api/hooks"
import type { SessionLedgerStatus } from "@/api/types"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Spinner } from "@/components/ui/spinner"
import { i18n } from "@/i18n/i18n"
import { holdsUnmergedChanges } from "@/lib/session-ledger"
import { projectPath } from "@/lib/tree"

/** The route of a session's page. */
const SESSION_ROUTE = "/p/:projectId/s/:sessionId"

/** The precondition type of a delete refused for unmerged Ledger changes. */
export const LEDGER_CHANGES = "LEDGER_CHANGES"

/**
 * Confirms deleting a stopped session, then deletes it and goes to its
 * project. A session with Ledger changes that aren't merged (as far as the
 * page knows, or as the server answers) asks again, saying they will be
 * discarded; confirming that deletes with the changes discarded.
 */
export function DeleteSessionDialog({
  open,
  onOpenChange,
  sessionId,
  projectId,
  ledger,
  disabledReason,
  onCloseAutoFocus,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  sessionId: string
  projectId: string
  ledger: SessionLedgerStatus | undefined
  /** Why deleting can't be done right now; null when it can. */
  disabledReason: string | null
  onCloseAutoFocus?: (event: Event) => void
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  // Where the page is now. When the user went elsewhere while the delete
  // was under way, this page is gone (it shows one session) and they stay
  // where they are.
  const location = useLocation()
  const here = useRef<string | null>(location.pathname)
  useEffect(() => {
    here.current = location.pathname
    return () => {
      here.current = null
    }
  }, [location.pathname])
  // Deleted: off to the project while the deleted session's page still
  // shows, whatever became of the dialog meanwhile.
  const deleteSession = useDeleteSession({
    onDeleted: (deleted) => {
      const match = here.current === null ? null : matchPath(SESSION_ROUTE, here.current)
      if (match?.params.sessionId === deleted) void navigate(projectPath(projectId))
    },
  })
  // The server refused for unmerged changes: they are discarded once confirmed.
  const [refused, setRefused] = useState(false)
  const discard = refused || holdsUnmergedChanges(ledger)

  const pending = deleteSession.isPending
  const change = (next: boolean) => {
    // A delete under way can't be called off: the dialog stays until it is answered.
    if (!next && pending) return
    if (!next) {
      setRefused(false)
      deleteSession.reset()
    }
    onOpenChange(next)
  }
  const confirm = () =>
    deleteSession.mutate(
      { sessionId, discardLedgerChanges: discard },
      {
        onSuccess: () => change(false),
        onError: (error) => {
          if (isPreconditionFailure(error, LEDGER_CHANGES)) {
            setRefused(true)
            deleteSession.reset()
          }
        },
      },
    )

  const error = deleteSession.error
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent
        showCloseButton={false}
        onCloseAutoFocus={onCloseAutoFocus}
        onEscapeKeyDown={(event) => pending && event.preventDefault()}
        onInteractOutside={(event) => pending && event.preventDefault()}
        data-ledger-warning={discard || undefined}
      >
        <DialogHeader>
          <DialogTitle>{discard ? t("session.delete.ledgerTitle") : t("session.delete.title")}</DialogTitle>
          <DialogDescription>{discard ? t("session.delete.ledgerBody") : t("session.delete.body")}</DialogDescription>
        </DialogHeader>
        {error !== null && (
          <p role="alert" className="text-sm text-destructive">
            {deleteErrorMessage(error)}
          </p>
        )}
        <DialogFooter>
          <DialogClose asChild>
            <Button variant="outline" disabled={pending}>
              {t("common.cancel")}
            </Button>
          </DialogClose>
          <Button
            variant="destructive"
            disabled={disabledReason !== null || pending}
            onClick={confirm}
          >
            {pending && <Spinner aria-hidden />}
            {discard ? t("session.delete.discardAndDelete") : t("session.delete.confirm")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** Why the delete failed: the session is still stopping or busy, it is gone, or the request failed. */
function deleteErrorMessage(error: unknown): string {
  if (isApiError(error, "failed_precondition")) return i18n.t("session.delete.notYet")
  if (isApiError(error, "not_found")) return i18n.t("session.actionError.gone")
  return errorMessage(error)
}
