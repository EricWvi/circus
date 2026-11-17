import { useTranslation } from "react-i18next"

import { AuthCard } from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"

/** Shown instead of react-router's raw error screen when a route throws. */
export function RouteError() {
  const { t } = useTranslation()
  return (
    <AuthCard>
      <div className="flex flex-col items-center gap-4">
        <p className="text-sm text-muted-foreground">{t("errors.routeFailed")}</p>
        <Button variant="outline" asChild>
          {/* A full load, so a broken router state can't stick around. */}
          <a href="/">{t("errors.goHome")}</a>
        </Button>
      </div>
    </AuthCard>
  )
}
