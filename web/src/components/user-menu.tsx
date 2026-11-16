import { ChevronsUpDownIcon, LogOutIcon, MonitorIcon, MoonIcon, SunIcon } from "lucide-react"

import { useLogout } from "@/api/hooks"
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
export function UserMenu({ username, className }: { username: string; className?: string }) {
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
              {username.slice(0, 1)}
            </span>
            <span className="flex-1 truncate">{username}</span>
            <ChevronsUpDownIcon className="text-muted-foreground" />
          </SidebarMenuButton>
        </DropdownMenuTrigger>
        <DropdownMenuContent side="top" align="start" {...focus.content}>
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
