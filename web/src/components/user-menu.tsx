import { ChevronsUpDownIcon, LanguagesIcon, LogOutIcon, SettingsIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"

import { useLogout } from "@/api/hooks"
import type { User } from "@/api/types"
import { isTheme, themeChoices } from "@/components/theme/theme-choices"
import { useTheme } from "@/components/theme/theme-context"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { SidebarMenuButton, useSidebar } from "@/components/ui/sidebar"
import { useMenuFocus } from "@/hooks/use-menu-focus"
import { isPreference, languageNames, languagePreferences } from "@/i18n/language"
import { setLanguagePreference, useLanguagePreference } from "@/i18n/preference"

/** The signed-in user's row in the sidebar footer, opening account options. */
export function UserMenu({ user, className }: { user: User; className?: string }) {
  const { t } = useTranslation()
  const { theme, setTheme } = useTheme()
  const language = useLanguagePreference()
  const logout = useLogout()
  const focus = useMenuFocus()
  const navigate = useNavigate()
  const { isMobile, setOpenMobile } = useSidebar()

  return (
    <>
      <DropdownMenu onOpenChange={(open) => open && logout.reset()}>
        <DropdownMenuTrigger asChild {...focus.trigger}>
          <SidebarMenuButton className={className}>
            <span
              className="flex size-6 shrink-0 items-center justify-center bg-sidebar-primary text-xs font-medium text-sidebar-primary-foreground uppercase"
              aria-hidden
            >
              {user.email.slice(0, 1).toUpperCase()}
            </span>
            <span className="flex-1 truncate">{user.email}</span>
            <ChevronsUpDownIcon className="text-muted-foreground" />
          </SidebarMenuButton>
        </DropdownMenuTrigger>
        <DropdownMenuContent side="top" align="start" {...focus.content}>
          <DropdownMenuLabel className="flex items-center gap-2 font-normal">
            <span className="min-w-0 truncate">{user.email}</span>
            {user.role === "super" && (
              <span className="shrink-0 rounded-sm border px-1 text-[0.625rem] leading-4">
                {t("userMenu.superUser")}
              </span>
            )}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          <DropdownMenuLabel>{t("userMenu.theme")}</DropdownMenuLabel>
          <DropdownMenuRadioGroup
            value={theme}
            onValueChange={(value) => isTheme(value) && setTheme(value)}
          >
            {themeChoices.map(({ value, icon: Icon }) => (
              <DropdownMenuRadioItem key={value} value={value}>
                <Icon />
                {t(`userMenu.themes.${value}`)}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
          <DropdownMenuSeparator />
          <DropdownMenuSub>
            <DropdownMenuSubTrigger>
              <LanguagesIcon />
              <span className="flex-1">{t("userMenu.language")}</span>
              <span className="text-xs text-muted-foreground">
                {language === "system" ? t("userMenu.systemLanguage") : languageNames[language]}
              </span>
            </DropdownMenuSubTrigger>
            <DropdownMenuSubContent>
              <DropdownMenuRadioGroup
                value={language}
                onValueChange={(value) => isPreference(value) && setLanguagePreference(value)}
              >
                {languagePreferences.map((choice) => (
                  // Language names are shown in their own language.
                  <DropdownMenuRadioItem
                    key={choice}
                    value={choice}
                    lang={choice === "system" ? undefined : choice}
                  >
                    {choice === "system" ? t("userMenu.systemLanguage") : languageNames[choice]}
                  </DropdownMenuRadioItem>
                ))}
              </DropdownMenuRadioGroup>
            </DropdownMenuSubContent>
          </DropdownMenuSub>
          <DropdownMenuSeparator />
          <DropdownMenuItem
            onSelect={() => {
              if (isMobile) setOpenMobile(false)
              void navigate("/settings")
            }}
          >
            <SettingsIcon />
            {t("userMenu.settings")}
          </DropdownMenuItem>
          <DropdownMenuItem onSelect={() => logout.mutate()}>
            <LogOutIcon />
            {t("userMenu.signOut")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {logout.isError && (
        <p role="alert" className="px-2 pt-1 text-xs text-destructive">
          {t("userMenu.signOutFailed")}
        </p>
      )}
    </>
  )
}
