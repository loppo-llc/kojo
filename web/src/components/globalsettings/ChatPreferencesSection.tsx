import { SectionCard } from "../ui/SectionCard";
import { Toggle } from "../ui/Toggle";

interface Props {
  enterSends: boolean;
  setEnterSends: (v: boolean) => void;
}

/** Chat preferences section — Enter key send behavior toggle. */
export function ChatPreferencesSection({ enterSends, setEnterSends }: Props) {
  return (
    <SectionCard title="Chat">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-[13px] text-ink">Send with Enter</div>
          <div className="mt-0.5 text-[12px] text-ink-faint">
            {enterSends
              ? "Enter to send, Shift+Enter for newline"
              : "Shift+Enter to send, Enter for newline"}
          </div>
        </div>
        <Toggle checked={enterSends} onChange={setEnterSends} aria-label="Send with Enter" />
      </div>
    </SectionCard>
  );
}
