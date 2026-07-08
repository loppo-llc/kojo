import { useT } from "../../lib/i18n";
import { useReloadPrompt, dismissReloadPrompt } from "../../lib/versionCheck";
import { Banner } from "./Banner";
import { Button } from "./Button";

/**
 * Non-intrusive banner shown when the running server reports a version
 * that differs from this loaded frontend bundle (a new deploy landed).
 * Rendered once at the app root; fixed to the bottom so it never shifts
 * layout. Never auto-reloads — the user may have an unsent draft.
 */
export function ReloadPrompt() {
  const t = useT();
  const pending = useReloadPrompt();
  if (pending === null) return null;

  return (
    <div className="pointer-events-none fixed inset-x-0 bottom-0 z-50 flex justify-center p-3">
      <div className="pointer-events-auto w-full max-w-md shadow-lg">
        <Banner
          tone="info"
          action={
            <div className="flex shrink-0 items-center gap-2">
              <button
                className="text-[13px] text-ink-faint hover:text-ink"
                onClick={dismissReloadPrompt}
              >
                {t("common.dismiss")}
              </button>
              <Button variant="primary" onClick={() => location.reload()}>
                {t("reload.action")}
              </Button>
            </div>
          }
        >
          {t("reload.available")}
        </Banner>
      </div>
    </div>
  );
}
