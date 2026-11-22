// Which model provider a session uses or would use, and how selects name
// the choices. Select values are provider ids, or one of the markers below
// (ids never start with ":").

import type { ModelProvider, Project } from "@/api/types"
import { i18n } from "@/i18n/i18n"

/** Local login: the worker machine's own Claude Code login. */
export const LOCAL_LOGIN = ":local"
/** No provider named: the project's default, else the user's, else local login. */
export const DEFAULT_CHOICE = ":default"
/** A project without a default provider. */
export const NO_PROVIDER = ":none"

/**
 * The provider a new session in `project` gets when it names none: the
 * project's default, else the user's (a default naming a deleted provider
 * counts as unset). Null: local login.
 */
export function defaultProvider(
  project: Pick<Project, "defaultProviderId">,
  providers: readonly ModelProvider[],
): { source: "project" | "user"; provider: ModelProvider } | null {
  const own = providers.find((p) => p.providerId === project.defaultProviderId)
  if (project.defaultProviderId !== "" && own) return { source: "project", provider: own }
  const mine = providers.find((p) => p.isDefault)
  return mine ? { source: "user", provider: mine } : null
}

/** The label of the "no provider named" choice, or null when it means local login. */
export function defaultChoiceLabel(
  project: Pick<Project, "defaultProviderId">,
  providers: readonly ModelProvider[],
): string | null {
  const d = defaultProvider(project, providers)
  if (!d) return null
  return d.source === "project"
    ? i18n.t("providers.select.projectDefault", { name: d.provider.name })
    : i18n.t("providers.select.userDefault", { name: d.provider.name })
}

/**
 * What the new-session select holds while nothing was picked: the default,
 * which reads as local login when there is none. It is never the explicit
 * local login choice, so picking that is a change and sends "".
 */
export const initialChoice = DEFAULT_CHOICE

/**
 * The request's `providerId` for a choice: undefined (the default) while
 * nothing was picked or the default is, "" for local login.
 */
export function requestedProvider(choice: string | null): string | undefined {
  if (choice === null || choice === DEFAULT_CHOICE) return undefined
  if (choice === LOCAL_LOGIN) return ""
  return choice
}

/** Whether a new session with this choice runs with local login. */
export function choiceIsLocalLogin(
  choice: string | null,
  project: Pick<Project, "defaultProviderId">,
  providers: readonly ModelProvider[],
): boolean {
  const value = choice ?? initialChoice
  return value === LOCAL_LOGIN || (value === DEFAULT_CHOICE && !defaultProvider(project, providers))
}

/** How a session's provider reads: its name, local login, or a deleted provider. */
export function sessionProviderName(
  providerId: string,
  providers: readonly ModelProvider[] | undefined,
): { label: string; deleted: boolean } {
  if (providerId === "") return { label: i18n.t("providers.select.localLogin"), deleted: false }
  const provider = providers?.find((p) => p.providerId === providerId)
  // Unknown until the list loads: not called deleted yet.
  if (!provider) {
    return providers ? { label: i18n.t("session.provider.deleted"), deleted: true } : { label: "", deleted: false }
  }
  return { label: provider.name, deleted: false }
}
