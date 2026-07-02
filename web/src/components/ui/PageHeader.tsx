interface PageHeaderProps {
  title: React.ReactNode;
  onBack: () => void;
  /** Right-aligned actions (e.g. an Add button). */
  children?: React.ReactNode;
  /** Optional row rendered under the title bar (e.g. mobile section nav). */
  below?: React.ReactNode;
}

/**
 * Sticky settings/detail page header with the app-shell back-arrow pattern.
 *
 * The back button's visible label is the literal "←" glyph (no aria-label)
 * so its accessible name stays "←" — several navigation tests select it via
 * getByRole("button", { name: "←" }).
 */
export function PageHeader({ title, onBack, children, below }: PageHeaderProps) {
  return (
    <header className="sticky top-0 z-40 border-b border-hairline bg-app/85 backdrop-blur">
      <div className="mx-auto flex h-14 max-w-[900px] items-center gap-2 px-4">
        <button
          onClick={onBack}
          className="-ml-2 rounded-[10px] p-2 text-lg leading-none text-ink-dim transition-colors hover:bg-hover hover:text-ink"
        >
          ←
        </button>
        <h1 className="min-w-0 flex-1 truncate text-[17px] font-semibold text-ink">
          {title}
        </h1>
        {children}
      </div>
      {below}
    </header>
  );
}
