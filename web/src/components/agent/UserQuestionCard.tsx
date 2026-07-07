import { useState } from "react";
import type { UserQuestion } from "../../lib/agentApi";
import { errMsg } from "../../lib/utils";

export interface PendingQuestion {
  requestId: string;
  questions: UserQuestion[];
}

// UserQuestionCard renders an interactive AskUserQuestion prompt raised by the
// agent's running turn. Each question offers its options as buttons (single
// select) or checkboxes (multiSelect), plus a free-text "その他" fallback. On
// submit it POSTs the answers; a 409/404 marks the card expired.
export function UserQuestionCard({
  pending,
  onSubmit,
}: {
  pending: PendingQuestion;
  onSubmit: (answers: Record<string, string | string[]>) => Promise<void>;
}) {
  const { questions } = pending;
  // Per-question selected labels (multi) / single label, plus free text.
  const [selected, setSelected] = useState<Record<number, string[]>>({});
  const [custom, setCustom] = useState<Record<number, string>>({});
  const [submitting, setSubmitting] = useState(false);
  const [done, setDone] = useState<string | null>(null);
  const [expired, setExpired] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const pick = (qi: number, label: string, multi: boolean) => {
    setSelected((prev) => {
      const cur = prev[qi] ?? [];
      if (multi) {
        return { ...prev, [qi]: cur.includes(label) ? cur.filter((l) => l !== label) : [...cur, label] };
      }
      return { ...prev, [qi]: [label] };
    });
  };

  const buildAnswers = (): Record<string, string | string[]> => {
    const out: Record<string, string | string[]> = {};
    questions.forEach((q, qi) => {
      const free = (custom[qi] ?? "").trim();
      const sel = selected[qi] ?? [];
      if (q.multiSelect) {
        const vals = free ? [...sel, free] : sel;
        out[q.question] = vals;
      } else {
        out[q.question] = free || sel[0] || "";
      }
    });
    return out;
  };

  const canSubmit = questions.every((q, qi) => {
    const free = (custom[qi] ?? "").trim();
    return free !== "" || (selected[qi] ?? []).length > 0;
  });

  const submit = async () => {
    if (submitting || !canSubmit) return;
    setSubmitting(true);
    setError(null);
    const answers = buildAnswers();
    try {
      await onSubmit(answers);
      setDone(
        questions
          .map((q) => {
            const v = answers[q.question];
            return `${q.header || q.question}: ${Array.isArray(v) ? v.join(", ") : v}`;
          })
          .join(" / "),
      );
    } catch (e) {
      const msg = errMsg(e);
      if (msg.startsWith("409:") || msg.startsWith("404:")) {
        setExpired(true);
      } else {
        setError(msg);
      }
    } finally {
      setSubmitting(false);
    }
  };

  if (done) {
    return (
      <div className="mx-auto max-w-[760px] rounded-[10px] border border-hairline bg-app/60 px-3 py-2 text-xs text-ink-dim">
        回答済み: {done}
      </div>
    );
  }
  if (expired) {
    return (
      <div className="mx-auto max-w-[760px] rounded-[10px] border border-hairline bg-app/60 px-3 py-2 text-xs text-ink-dim">
        この質問は期限切れ (ターンが終了した)。
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-[760px] rounded-[12px] border border-lamp-warn/40 bg-lamp-warn/5 px-4 py-3">
      <div className="mb-2 text-xs font-medium text-lamp-warn">質問</div>
      {questions.map((q, qi) => (
        <div key={qi} className="mb-3 last:mb-0">
          {q.header && <div className="mb-1 text-xs text-ink-dim">{q.header}</div>}
          <div className="mb-2 text-sm text-ink">{q.question}</div>
          <div className="flex flex-wrap gap-2">
            {(q.options ?? []).map((opt) => {
              const on = (selected[qi] ?? []).includes(opt.label);
              return (
                <button
                  key={opt.label}
                  type="button"
                  title={opt.description}
                  onClick={() => pick(qi, opt.label, !!q.multiSelect)}
                  className={
                    "rounded-[8px] border px-3 py-1.5 text-sm transition " +
                    (on
                      ? "border-copper bg-copper/15 text-copper-bright"
                      : "border-hairline bg-app text-ink hover:border-copper/60")
                  }
                >
                  {q.multiSelect && (
                    <span className="mr-1.5 inline-block">{on ? "☑" : "☐"}</span>
                  )}
                  {opt.label}
                </button>
              );
            })}
          </div>
          <input
            type="text"
            value={custom[qi] ?? ""}
            onChange={(e) => setCustom((prev) => ({ ...prev, [qi]: e.target.value }))}
            placeholder="その他 (自由入力)"
            className="mt-2 w-full rounded-[8px] border border-hairline bg-app px-3 py-1.5 text-sm text-ink outline-none focus:border-copper"
          />
        </div>
      ))}
      {error && <div className="mb-2 text-xs text-lamp-err">{error}</div>}
      <button
        type="button"
        disabled={!canSubmit || submitting}
        onClick={submit}
        className="rounded-[8px] bg-copper px-4 py-1.5 text-sm font-semibold text-[#14100b] transition-colors hover:bg-copper-bright disabled:opacity-40"
      >
        {submitting ? "送信中…" : "回答する"}
      </button>
    </div>
  );
}
