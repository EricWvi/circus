import { MessageSquareDashedIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"

import type { Project, SessionSummary } from "@/api/types"
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation"
import { Shimmer } from "@/components/ai-elements/shimmer"
import { PageHeader } from "@/components/page-header"
import { StatusBadge } from "@/components/status-dot"
import { projectPath } from "@/lib/tree"
import { useConversations } from "@/state/conversations-context"

import { EventItem } from "./event-list"
import { SessionPrompt } from "./session-prompt"

export function SessionView({
  project,
  session,
}: {
  project: Project
  session: SessionSummary
}) {
  const { t } = useTranslation()
  const { events: eventsOf, send, stop, decideApproval } = useConversations()
  const events = eventsOf(session.sessionId)
  const last = events.at(-1)
  const streamingText = last?.kind === "assistant_text" && last.streaming && last.text !== ""
  const busy = session.state === "running"
  const showWorking = busy && !streamingText && last?.kind !== "tool_call"

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          <span className="hidden shrink-0 text-xs text-muted-foreground md:inline">
            {t("session.onDevice", { device: session.device })}
          </span>
        }
      >
        <Link
          to={projectPath(project.projectId)}
          className="hidden shrink-0 rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 sm:inline"
        >
          {project.name}
        </Link>
        <span className="hidden text-muted-foreground sm:inline" aria-hidden>
          /
        </span>
        <h1 className="min-w-0 truncate font-medium">{session.title}</h1>
        <StatusBadge state={session.state} />
      </PageHeader>

      <Conversation className="min-h-0">
        <ConversationContent className="mx-auto w-full max-w-3xl gap-6 px-4 py-6">
          {events.length === 0 ? (
            <ConversationEmptyState
              className="min-h-[50vh]"
              icon={<MessageSquareDashedIcon className="size-8" />}
              title={t("session.noMessages")}
              description={t("session.noMessagesHint", { device: session.device })}
            />
          ) : (
            events.map((event) => (
              <EventItem
                key={event.id}
                event={event}
                deviceName={session.device}
                onDecide={(eventId, decision) =>
                  decideApproval(session.sessionId, eventId, decision)
                }
              />
            ))
          )}
          {showWorking && (
            <div className="text-sm">
              <Shimmer>{t("session.working")}</Shimmer>
            </div>
          )}
        </ConversationContent>
        <ConversationScrollButton aria-label={t("session.scrollToLatest")} />
      </Conversation>

      <div className="mx-auto w-full max-w-3xl shrink-0 px-4 pb-4">
        <SessionPrompt
          busy={busy}
          onSend={(text, attachments) => send(session.sessionId, text, attachments)}
          onStop={() => stop(session.sessionId)}
        />
      </div>
    </div>
  )
}
