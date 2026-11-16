import { ChevronsUpDownIcon, LogOutIcon, MonitorIcon, MoonIcon, SunIcon } from "lucide-react"

import { useLogout } from "@/api/hooks"
import type { User } from "@/api/types"
import { useTheme, type Theme } from "@/components/theme/theme-context"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { SidebarMenuButton } from "@/components/ui/sidebar"
import { useMenuFocus } from "@/hooks/use-menu-focus"

const themes: { value: Theme; label: string; icon: typeof SunIcon }[] = [
  { value: "light", label: "Light", icon: SunIcon },
  { value: "dark", label: "Dark", icon: MoonIcon },
  { value: "system", label: "System", icon: MonitorIcon },
]

const isTheme = (value: string): value is Theme => themes.some((t) => t.value === value)

/** The signed-in user's row in the sidebar footer, opening account options. */
export function UserMenu({ user, className }: { user: User; className?: string }) {
  const { theme, setTheme } = useTheme()
  const logout = useLogout()
  const focus = useMenuFocus()

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
                Super user
              </span>
            )}
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          <DropdownMenuLabel>Theme</DropdownMenuLabel>
          <DropdownMenuRadioGroup
            value={theme}
            onValueChange={(value) => isTheme(value) && setTheme(value)}
          >
            {themes.map(({ value, label, icon: Icon }) => (
              <DropdownMenuRadioItem key={value} value={value}>
                <Icon />
                {label}
              </DropdownMenuRadioItem>
            ))}
          </DropdownMenuRadioGroup>
          <DropdownMenuSeparator />
          <DropdownMenuItem onSelect={() => logout.mutate()}>
            <LogOutIcon />
            Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      {logout.isError && (
        <p role="alert" className="px-2 pt-1 text-xs text-destructive">
          Couldn&apos;t sign out. Try again.
        </p>
      )}
    </>
  )
}
