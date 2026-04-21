interface Props {
  enterSends: boolean;
  setEnterSends: (v: boolean) => void;
}

/** Chat preferences section — Enter key send behavior toggle. */
export function ChatPreferencesSection({ enterSends, setEnterSends }: Props) {
  return (
    <div>
      <h2 className="text-xs font-semibold text-neutral-500 uppercase tracking-wider mb-3">
        Chat
      </h2>
      <div className="p-3 bg-neutral-900 border border-neutral-800 rounded-lg">
        <div className="flex items-center justify-between">
          <div>
            <div className="text-sm font-medium">Send with Enter</div>
            <div className="text-xs text-neutral-500 mt-0.5">
              {enterSends
                ? "Enter to send, Shift+Enter for newline"
                : "Shift+Enter to send, Enter for newline"}
            </div>
          </div>
          <button
            onClick={() => setEnterSends(!enterSends)}
            className={`relative w-10 h-5 rounded-full transition-colors ${enterSends ? "bg-blue-600" : "bg-neutral-700"}`}
            role="switch"
            aria-checked={enterSends}
          >
            <span
              className={`absolute top-0.5 left-0.5 w-4 h-4 rounded-full bg-white transition-transform ${enterSends ? "translate-x-5" : ""}`}
            />
          </button>
        </div>
      </div>
    </div>
  );
}
