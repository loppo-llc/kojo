import { Children, isValidElement, cloneElement, createContext, useContext, useMemo, useState, useCallback } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import remarkBreaks from "remark-breaks";

const InsideLinkCtx = createContext(false);
const InsidePreCtx = createContext(false);

interface MarkdownRendererProps {
  content: string;
  /** Optional callback to transform plain-text segments (e.g. file-path chips). */
  processText?: (text: string) => React.ReactNode;
}

/** Skip text processing inside these intrinsic element types. */
const SKIP_TAGS = new Set(["code", "pre", "a"]);

/**
 * Recursively apply processText to string children.
 * Skips code/pre/a intrinsic elements and all custom (function) components.
 * Only recurses into intrinsic HTML elements like strong, em, span, etc.
 */
function mapTextChildren(
  children: React.ReactNode,
  processText: (text: string) => React.ReactNode,
): React.ReactNode {
  return Children.map(children, (child) => {
    if (typeof child === "string") return processText(child);
    if (isValidElement(child)) {
      // Only recurse into intrinsic HTML elements (string type), not custom components
      if (typeof child.type !== "string") return child;
      if (SKIP_TAGS.has(child.type)) return child;
      const props = child.props as { children?: React.ReactNode };
      if (props.children != null) {
        return cloneElement(child, {}, mapTextChildren(props.children, processText));
      }
    }
    return child;
  });
}

export function MarkdownRenderer({ content, processText }: MarkdownRendererProps) {
  const components = useMemo(
    () => {
      const withText = (Tag: string) => {
        if (!processText) return undefined;
        return ({ children, node: _node, ...props }: any) => {
          const El = Tag as any;
          return <El {...props}>{mapTextChildren(children, processText)}</El>;
        };
      };

      return {
        pre({ children, ...props }: React.ComponentProps<"pre">) {
          // Extract language and code text from code child
          let lang = "";
          let codeText = "";
          const child = Array.isArray(children) ? children[0] : children;
          if (child && typeof child === "object" && "props" in child) {
            const childProps = (child as React.ReactElement<{ className?: string; children?: React.ReactNode }>).props;
            lang = (childProps.className || "").replace("language-", "");
            if (typeof childProps.children === "string") {
              codeText = childProps.children;
            }
          }
          return (
            <InsidePreCtx.Provider value={true}>
              <div className="md-code-wrap">
                {(lang || codeText) && (
                  <div className="md-code-header">
                    {lang && <div className="md-code-lang">{lang}</div>}
                    {codeText && <CodeCopyButton text={codeText} />}
                  </div>
                )}
                <pre {...props}>{children}</pre>
              </div>
            </InsidePreCtx.Provider>
          );
        },
        a({ children, href, ...props }: React.ComponentProps<"a">) {
          return (
            <InsideLinkCtx.Provider value={true}>
              <a
                href={href}
                target="_blank"
                rel="noopener noreferrer"
                className="md-link"
                {...props}
              >
                {children}
              </a>
            </InsideLinkCtx.Provider>
          );
        },
        ...(processText && {
          p: withText("p"),
          li: withText("li"),
          td: withText("td"),
          th: withText("th"),
          h1: withText("h1"),
          h2: withText("h2"),
          h3: withText("h3"),
          h4: withText("h4"),
          h5: withText("h5"),
          h6: withText("h6"),
          blockquote: withText("blockquote"),
          code({ children, node: _node, className, ...props }: any) {
            const insideLink = useContext(InsideLinkCtx);
            const insidePre = useContext(InsidePreCtx);
            // Only process inline code (skip code blocks and code inside links).
            // Replace only when the entire content is a single file path.
            const text = typeof children === "string" ? children
              : Array.isArray(children) && children.length === 1 && typeof children[0] === "string" ? children[0]
              : null;
            if (!insidePre && !insideLink && text) {
              const result = processText(text);
              if (Array.isArray(result) && result.length === 1 && typeof result[0] !== "string") {
                return <>{result}</>;
              }
            }
            return <code className={className} {...props}>{children}</code>;
          },
        }),
      };
    },
    [processText],
  );

  return (
    <div className="md-content">
      <ReactMarkdown remarkPlugins={[remarkGfm, remarkBreaks]} components={components}>
        {content}
      </ReactMarkdown>
    </div>
  );
}

/**
 * copyToClipboard writes `text` to the user's clipboard, falling back to a
 * legacy execCommand path when the async Clipboard API is unavailable
 * (non-secure contexts, older browsers, missing permissions). Returns true
 * when the copy appears to have succeeded.
 */
async function copyToClipboard(text: string): Promise<boolean> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // fall through to legacy fallback
    }
  }

  if (typeof document === "undefined") return false;

  const ta = document.createElement("textarea");
  ta.value = text;
  ta.setAttribute("readonly", "");
  ta.style.position = "fixed";
  ta.style.top = "-9999px";
  ta.style.opacity = "0";
  document.body.appendChild(ta);
  try {
    ta.select();
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    // Always remove the textarea, even if execCommand threw.
    ta.remove();
  }
}

/** Small copy button rendered inside code blocks. */
function CodeCopyButton({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  const [failed, setFailed] = useState(false);
  const handleCopy = useCallback(() => {
    // void: deliberately not awaiting — the promise's result flows through
    // setState below, and an unhandled rejection is not possible because
    // copyToClipboard catches internally.
    void copyToClipboard(text).then((ok) => {
      if (ok) {
        setCopied(true);
        setFailed(false);
        setTimeout(() => setCopied(false), 1500);
      } else {
        setFailed(true);
        setTimeout(() => setFailed(false), 1500);
      }
    });
  }, [text]);

  const title = failed ? "Copy failed" : "Copy code";
  return (
    <button
      onClick={handleCopy}
      className="md-code-copy"
      title={title}
      aria-label={title}
    >
      {copied ? (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M13.25 4.75 6 12 2.75 8.75" />
        </svg>
      ) : failed ? (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="8" cy="8" r="6.5" />
          <path d="M8 5v3.5M8 11v.01" />
        </svg>
      ) : (
        <svg width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <rect x="5.5" y="5.5" width="8" height="8" rx="1.5" />
          <path d="M10.5 5.5V3a1.5 1.5 0 0 0-1.5-1.5H3A1.5 1.5 0 0 0 1.5 3v6A1.5 1.5 0 0 0 3 10.5h2.5" />
        </svg>
      )}
    </button>
  );
}
