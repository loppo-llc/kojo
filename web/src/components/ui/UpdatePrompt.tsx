import { useEffect } from "react";
import { useT } from "../../lib/i18n";
import {
  dismissUpdatePrompt,
  fetchUpdateStatus,
  startUpdate,
  useUpdatePrompt,
} from "../../lib/updateCheck";
import { Banner } from "./Banner";
import { Button } from "./Button";

const POLL_MS = 60 * 60 * 1000; // 60 minutes — local daemon only

/**
 * Banner when the daemon reports a newer GitHub release. Fixed bottom
 * stack is owned by the app root wrapper (with ReloadPrompt) so the two
 * banners never overlap. Update POSTs self-update; after restart,
 * ReloadPrompt covers the frontend-bundle skew reload.
 */
export function UpdatePrompt() {
  const t = useT();
  const state = useUpdatePrompt();

  useEffect(() => {
    void fetchUpdateStatus();
    const id = window.setInterval(() => void fetchUpdateStatus(), POLL_MS);
    return () => window.clearInterval(id);
  }, []);

  if (state === null) return null;

  const pending = state.phase === "pending";

  let body: React.ReactNode;
  if (pending) {
    body = t("update.updating");
  } else {
    body = (
      <span className="flex flex-col gap-0.5">
        <span>
          {t("update.available", { latest: state.latest })}
          {state.notesUrl ? (
            <>
              {" "}
              <a
                href={state.notesUrl}
                target="_blank"
                rel="noreferrer"
                className="underline decoration-hairline underline-offset-2 hover:text-ink"
              >
                {t("update.notes")}
              </a>
            </>
          ) : null}
        </span>
        {!state.supported ? (
          <span className="text-ink-faint">{t("update.unsupported")}</span>
        ) : null}
        {state.phase === "error" && state.error ? (
          <span className="text-lamp-err">{state.error}</span>
        ) : null}
      </span>
    );
  }

  return (
    <div className="pointer-events-auto w-full max-w-md shadow-lg">
      <Banner
        tone="info"
        action={
          pending ? undefined : (
            <div className="flex shrink-0 items-center gap-2">
              <button
                type="button"
                className="text-[13px] text-ink-faint hover:text-ink"
                onClick={dismissUpdatePrompt}
              >
                {t("common.dismiss")}
              </button>
              {state.supported ? (
                <Button variant="primary" onClick={() => void startUpdate()}>
                  {t("update.action")}
                </Button>
              ) : null}
            </div>
          )
        }
      >
        {body}
      </Banner>
    </div>
  );
}
