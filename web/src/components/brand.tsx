import { cn } from "@/lib/utils"

/** The product mark and name. Takes the surrounding text color. */
export function Brand({ className }: { className?: string }) {
  return (
    <span className={cn("flex items-center gap-2", className)}>
      <span className="size-3 bg-current" aria-hidden />
      <span className="text-sm font-semibold tracking-tight">Circus</span>
    </span>
  )
}
